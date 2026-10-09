package model

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/signal"
)

const maxModelBytes = 512 << 20

var safeModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var (
	ErrInvalidID   = errors.New("invalid model id")
	ErrModelExists = errors.New("model already exists")
)

type ModelInfo struct {
	ID                 string   `json:"id"`
	Filename           string   `json:"filename"`
	Version            string   `json:"version"`
	Description        string   `json:"description"`
	SizeBytes          int64    `json:"size_bytes"`
	CreatedAt          string   `json:"created_at"`
	PCKScore           *float64 `json:"pck_score"`
	HasQuantization    bool     `json:"has_quantization"`
	LoRAProfiles       []string `json:"lora_profiles"`
	SegmentCount       int      `json:"segment_count"`
	Loadable           bool     `json:"loadable"`
	Architecture       string   `json:"architecture,omitempty"`
	InferenceAvailable bool     `json:"inference_available"`
}

type ActiveModel struct {
	ModelID            string   `json:"model_id"`
	Filename           string   `json:"filename"`
	Version            string   `json:"version"`
	Description        string   `json:"description"`
	AvgInferenceMS     float64  `json:"avg_inference_ms"`
	FramesProcessed    uint64   `json:"frames_processed"`
	WeightCount        int      `json:"weight_count"`
	PoseSource         string   `json:"pose_source"`
	InferenceAvailable bool     `json:"inference_available"`
	Architecture       string   `json:"architecture,omitempty"`
	ValidationAccuracy *float64 `json:"validation_accuracy,omitempty"`
	ValidationSamples  int      `json:"validation_samples,omitempty"`
	ValidationSplit    string   `json:"validation_split,omitempty"`
	LoRAProfiles       []string `json:"lora_profiles"`
	ActiveLoRAProfile  *string  `json:"active_lora_profile"`
}

type loadedModel struct {
	info            ModelInfo
	weightCount     int
	container       *Container
	activeLoRA      *string
	predictor       *adaptive.Model
	framesProcessed atomic.Uint64
	inferenceNanos  atomic.Uint64
}

type Registry struct {
	root   string
	mu     sync.RWMutex
	active *loadedModel
}

func NewRegistry(root string) (*Registry, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("model directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve model directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create model directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("model directory must be a real directory")
	}
	return &Registry{root: absolute}, nil
}

func (r *Registry) List() ([]ModelInfo, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, fmt.Errorf("scan model directory: %w", err)
	}
	models := make([]ModelInfo, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".rvf") || !entry.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !validID(id) {
			continue
		}
		path := filepath.Join(r.root, name)
		fileInfo, statErr := os.Lstat(path)
		if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() < 0 || fileInfo.Size() > maxModelBytes {
			continue
		}
		info := ModelInfo{ID: id, Filename: name, Version: "unknown", SizeBytes: fileInfo.Size(), LoRAProfiles: []string{}}
		if container, parseErr := readContainer(path); parseErr == nil {
			info = modelInfoFromContainer(id, name, fileInfo.Size(), fileInfo.ModTime(), container)
			_, supported, compileErr := compileActivity(container)
			info.InferenceAvailable = supported && compileErr == nil
			info.Loadable = container.ValidateWeights() == nil && compileErr == nil
		}
		models = append(models, info)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func (r *Registry) Get(id string) (ModelInfo, error) {
	path, filename, err := r.modelPath(id)
	if err != nil {
		return ModelInfo{}, err
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return ModelInfo{}, err
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Size() < 0 || fileInfo.Size() > maxModelBytes {
		return ModelInfo{}, errors.New("model file is not a supported regular file")
	}
	container, err := readContainer(path)
	if err != nil {
		return ModelInfo{}, err
	}
	info := modelInfoFromContainer(id, filename, fileInfo.Size(), fileInfo.ModTime(), container)
	_, supported, compileErr := compileActivity(container)
	info.InferenceAvailable = supported && compileErr == nil
	if err := container.ValidateWeights(); err != nil {
		compileErr = err
	}
	info.Loadable = compileErr == nil
	return info, nil
}

func (r *Registry) Load(id string) (ActiveModel, error) {
	path, filename, err := r.modelPath(id)
	if err != nil {
		return ActiveModel{}, err
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return ActiveModel{}, err
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Size() < 0 || fileInfo.Size() > maxModelBytes {
		return ActiveModel{}, errors.New("model file is not a supported regular file")
	}
	container, err := readContainer(path)
	if err != nil {
		return ActiveModel{}, err
	}
	if err := container.ValidateWeights(); err != nil {
		return ActiveModel{}, fmt.Errorf("model is not loadable: %w", err)
	}
	predictor, inferenceAvailable, err := compileActivity(container)
	if err != nil {
		return ActiveModel{}, fmt.Errorf("model inference contract is invalid: %w", err)
	}
	info := modelInfoFromContainer(id, filename, fileInfo.Size(), fileInfo.ModTime(), container)
	info.Loadable = true
	info.InferenceAvailable = inferenceAvailable
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = &loadedModel{
		info: info, weightCount: container.WeightCount(), container: container,
		predictor: predictor,
	}
	result := activeInfo(r.active)
	return result, nil
}

func (r *Registry) Unload() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return "", errors.New("no model is currently loaded")
	}
	id := r.active.info.ID
	r.active = nil
	return id, nil
}

func (r *Registry) Active() (ActiveModel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return ActiveModel{}, false
	}
	return activeInfo(r.active), true
}

// ApplyObservation applies the active executable RVF model to a raw CSI
// observation. Models with an unknown architecture remain inspectable but do
// not modify inference. The return value reports whether a model was applied.
func (r *Registry) ApplyObservation(observation *signal.Observation) (bool, error) {
	if r == nil || observation == nil {
		return false, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	active := r.active
	if active == nil || active.predictor == nil {
		return false, nil
	}
	started := time.Now()
	err := active.predictor.Apply(observation)
	active.inferenceNanos.Add(uint64(time.Since(started).Nanoseconds()))
	if err != nil {
		return true, err
	}
	active.framesProcessed.Add(1)
	return true, nil
}

func (r *Registry) ActivateLoRA(modelID, profile string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return errors.New("no model is currently loaded")
	}
	if modelID != r.active.info.ID {
		return fmt.Errorf("model %q is not loaded", modelID)
	}
	profile = strings.TrimSpace(profile)
	for _, name := range r.active.info.LoRAProfiles {
		if name == profile {
			copy := profile
			r.active.activeLoRA = &copy
			return nil
		}
	}
	return fmt.Errorf("LoRA profile %q is not available on the active model", profile)
}

func (r *Registry) LoRAProfiles() (string, []string, *string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return "", []string{}, nil
	}
	return r.active.info.ID, append([]string(nil), r.active.info.LoRAProfiles...), cloneString(r.active.activeLoRA)
}

func (r *Registry) ActiveContainerInfo() (ContainerInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return ContainerInfo{}, false
	}
	return r.active.container.Info(), true
}

func (r *Registry) ActiveSegments() ([]map[string]any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return nil, false
	}
	segments := make([]map[string]any, 0, len(r.active.container.Segments))
	for _, segment := range r.active.container.Segments {
		segments = append(segments, map[string]any{
			"segment_id": segment.ID, "type": segment.Type, "size_bytes": len(segment.Payload),
		})
	}
	return segments, true
}

func (r *Registry) Delete(id string) error {
	path, _, err := r.modelPath(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil && r.active.info.ID == id {
		return errors.New("unload the model before deleting it")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing to delete non-regular model file")
	}
	return os.Remove(path)
}

// InstallGenerated atomically installs a newly trained RVF model without
// replacing an existing artifact. Only a supported executable architecture is
// accepted, and a generated model may not bypass the same validation as load.
func (r *Registry) InstallGenerated(id string, data []byte) error {
	path, _, err := r.modelPath(id)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxModelBytes {
		return errors.New("generated model exceeds the supported file size")
	}
	container, err := ParseRVF(data)
	if err != nil {
		return fmt.Errorf("parse generated model: %w", err)
	}
	if manifestID, _ := container.Manifest()["model_id"].(string); manifestID != id {
		return errors.New("generated model id does not match its destination")
	}
	if err := container.ValidateWeights(); err != nil {
		return fmt.Errorf("generated model has invalid weights: %w", err)
	}
	if _, supported, err := compileActivity(container); err != nil || !supported {
		if err != nil {
			return fmt.Errorf("generated model has an invalid inference contract: %w", err)
		}
		return errors.New("generated model does not use a supported inference architecture")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("model destination is not a regular file")
		}
		return ErrModelExists
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect model destination: %w", statErr)
	}
	tmp, err := os.CreateTemp(r.root, ".model-*.tmp")
	if err != nil {
		return fmt.Errorf("create generated model temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure generated model: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write generated model: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync generated model: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close generated model: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrModelExists
		}
		return fmt.Errorf("install generated model: %w", err)
	}
	directory, err := os.Open(r.root)
	if err != nil {
		return fmt.Errorf("open model directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync model directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close model directory: %w", err)
	}
	return nil
}

func (r *Registry) modelPath(id string) (string, string, error) {
	if !validID(id) {
		return "", "", ErrInvalidID
	}
	filename := id + ".rvf"
	return filepath.Join(r.root, filename), filename, nil
}

func (r *Registry) readModel(id string) (*Container, string, os.FileInfo, error) {
	path, filename, err := r.modelPath(id)
	if err != nil {
		return nil, "", nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxModelBytes {
		return nil, "", nil, errors.New("model file is not a supported regular file")
	}
	container, err := readContainer(path)
	return container, filename, info, err
}

func readContainer(path string) (*Container, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxModelBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read model: %w", err)
	}
	if len(data) > maxModelBytes {
		return nil, errors.New("model exceeds maximum supported size")
	}
	return ParseRVF(data)
}

func modelInfoFromContainer(id, filename string, size int64, modified time.Time, container *Container) ModelInfo {
	manifest := container.Manifest()
	metadata := container.Metadata()
	info := ModelInfo{
		ID: id, Filename: filename, Version: "unknown", SizeBytes: size,
		LoRAProfiles: container.LoRAProfiles(), SegmentCount: len(container.Segments),
		HasQuantization: container.Find(segmentQuant) != nil,
	}
	if value, ok := manifest["version"].(string); ok && value != "" {
		info.Version = value
	}
	if value, ok := manifest["description"].(string); ok {
		info.Description = value
	}
	if value, ok := manifest["created_at"].(string); ok {
		info.CreatedAt = value
	}
	if inference, ok := manifest["inference"].(map[string]any); ok {
		if architecture, ok := inference["architecture"].(string); ok {
			info.Architecture = architecture
		}
	}
	if info.CreatedAt == "" && !modified.IsZero() {
		info.CreatedAt = modified.UTC().Format(time.RFC3339)
	}
	if training, ok := metadata["training"].(map[string]any); ok {
		if score, ok := training["best_pck"].(float64); ok && !math.IsNaN(score) && !math.IsInf(score, 0) {
			info.PCKScore = &score
		}
	}
	return info
}

func activeInfo(active *loadedModel) ActiveModel {
	frames := active.framesProcessed.Load()
	avgMS := 0.0
	if frames > 0 {
		avgMS = float64(active.inferenceNanos.Load()) / float64(frames) / float64(time.Millisecond)
	}
	poseSource := "not_connected"
	var validationAccuracy *float64
	validationSamples := 0
	validationSplit := ""
	if active.predictor != nil {
		poseSource = "activity_classifier"
		value := active.predictor.ValidationAccuracy
		if active.predictor.ValidationSamples > 0 {
			validationAccuracy = &value
		}
		validationSamples = active.predictor.ValidationSamples
		validationSplit = active.predictor.ValidationSplit
	}
	return ActiveModel{
		ModelID: active.info.ID, Filename: active.info.Filename, Version: active.info.Version,
		Description: active.info.Description, AvgInferenceMS: avgMS, FramesProcessed: frames, WeightCount: active.weightCount,
		PoseSource: poseSource, InferenceAvailable: active.predictor != nil, Architecture: active.info.Architecture,
		ValidationAccuracy: validationAccuracy, ValidationSamples: validationSamples, ValidationSplit: validationSplit,
		LoRAProfiles: append([]string(nil), active.info.LoRAProfiles...), ActiveLoRAProfile: cloneString(active.activeLoRA),
	}
}

func validID(id string) bool { return safeModelID.MatchString(id) && id != "." && id != ".." }

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// ModelIDsFromList returns IDs only; it is useful for narrow API serializers.
func ModelIDsFromList(models []ModelInfo) []string {
	ids := make([]string, len(models))
	for index, item := range models {
		ids[index] = item.ID
	}
	return ids
}
