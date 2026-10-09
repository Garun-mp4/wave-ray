// Package training owns supervised activity-model jobs and the dataset split
// policy used to produce executable RVF artifacts.
package training

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/model"
)

const defaultEpochs = 200

type Config struct {
	Epochs       int
	BatchSize    int
	LearningRate float64
	WeightDecay  float64
	Patience     int
}

var (
	ErrTrainingActive = errors.New("a model training job is already active")
	ErrNoActiveJob    = errors.New("no model training job is active")
	ErrNoModelStore   = errors.New("model registry is unavailable")
)

type Status struct {
	Enabled              bool      `json:"enabled"`
	PoseTrainingEnabled  bool      `json:"pose_training_enabled"`
	SupportedTasks       []string  `json:"supported_tasks"`
	Active               bool      `json:"active"`
	JobID                string    `json:"job_id,omitempty"`
	Task                 string    `json:"task,omitempty"`
	Phase                string    `json:"phase"`
	Epoch                int       `json:"epoch"`
	TotalEpochs          int       `json:"total_epochs"`
	TrainSamples         int       `json:"train_samples,omitempty"`
	ValidationSamples    int       `json:"validation_samples,omitempty"`
	TrainAccuracy        *float64  `json:"train_accuracy_in_sample,omitempty"`
	ValidationAccuracy   *float64  `json:"validation_accuracy,omitempty"`
	ValidationSplit      string    `json:"validation_split,omitempty"`
	DatasetIDs           []string  `json:"dataset_ids,omitempty"`
	TrainingDatasetIDs   []string  `json:"training_dataset_ids,omitempty"`
	ValidationDatasetIDs []string  `json:"validation_dataset_ids,omitempty"`
	ModelID              string    `json:"model_id,omitempty"`
	Error                string    `json:"error,omitempty"`
	StartedAt            time.Time `json:"started_at,omitempty"`
	FinishedAt           time.Time `json:"finished_at,omitempty"`
}

type job struct {
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

type Manager struct {
	recordingsDir string
	models        *model.Registry
	mu            sync.Mutex
	current       *job
	sequence      atomic.Uint64
}

func New(dataDir string, models *model.Registry) (*Manager, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("training data directory is required")
	}
	recordingsDir, err := filepath.Abs(filepath.Join(dataDir, "recordings"))
	if err != nil {
		return nil, fmt.Errorf("resolve training recordings directory: %w", err)
	}
	if err := os.MkdirAll(recordingsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create training recordings directory: %w", err)
	}
	info, err := os.Lstat(recordingsDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("training recordings path must be a real directory")
	}
	return &Manager{recordingsDir: recordingsDir, models: models}, nil
}

func (m *Manager) Datasets() ([]adaptive.TrainingDataset, error) {
	if m == nil {
		return nil, errors.New("training manager is unavailable")
	}
	return adaptive.ListTrainingDatasets(m.recordingsDir)
}

func (m *Manager) Dataset(id string) (adaptive.TrainingDataset, error) {
	datasets, err := m.Datasets()
	if err != nil {
		return adaptive.TrainingDataset{}, err
	}
	for _, dataset := range datasets {
		if dataset.ID == id {
			return dataset, nil
		}
	}
	return adaptive.TrainingDataset{}, os.ErrNotExist
}

func (m *Manager) Start(datasetIDs []string, config Config) (Status, error) {
	if m == nil || m.models == nil {
		return Status{}, ErrNoModelStore
	}
	if config.Epochs == 0 {
		config.Epochs = defaultEpochs
	}
	if config.BatchSize == 0 {
		config.BatchSize = 32
	}
	if config.LearningRate == 0 {
		config.LearningRate = 0.1
	}
	if config.WeightDecay == 0 {
		config.WeightDecay = 1e-4
	}
	if config.Epochs < 1 || config.Epochs > 1000 || config.BatchSize < 1 || config.BatchSize > 1024 || math.IsNaN(config.LearningRate) || math.IsInf(config.LearningRate, 0) || config.LearningRate <= 0 || config.LearningRate > 1 || math.IsNaN(config.WeightDecay) || math.IsInf(config.WeightDecay, 0) || config.WeightDecay < 0 || config.WeightDecay > 1 || config.Patience < 0 || config.Patience > 1000 {
		return Status{}, errors.New("invalid model training configuration")
	}
	datasets, err := m.Datasets()
	if err != nil {
		return Status{}, err
	}
	datasetByID := make(map[string]adaptive.TrainingDataset, len(datasets))
	for _, dataset := range datasets {
		datasetByID[dataset.ID] = dataset
	}
	trainingIDs, validationIDs, err := splitDatasets(datasetIDs, func(id string) (string, bool, error) {
		dataset, exists := datasetByID[id]
		if !exists {
			return "", false, os.ErrNotExist
		}
		return dataset.Label, dataset.Eligible, nil
	})
	if err != nil {
		return Status{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	newJob := &job{
		status: Status{
			Enabled: true, PoseTrainingEnabled: false, SupportedTasks: []string{"activity_classifier"},
			Active: true, JobID: fmt.Sprintf("train-%d-%d", time.Now().UTC().UnixNano(), m.sequence.Add(1)),
			Task: "activity_classifier", Phase: "loading", TotalEpochs: config.Epochs,
			DatasetIDs:           append([]string(nil), datasetIDs...),
			TrainingDatasetIDs:   append([]string(nil), trainingIDs...),
			ValidationDatasetIDs: append([]string(nil), validationIDs...), StartedAt: time.Now().UTC(),
		},
		cancel: cancel, done: make(chan struct{}),
	}
	startingStatus := cloneStatus(newJob.status)
	m.mu.Lock()
	if m.current != nil && m.current.status.Active {
		m.mu.Unlock()
		cancel()
		return Status{}, ErrTrainingActive
	}
	m.current = newJob
	m.mu.Unlock()
	go m.run(ctx, newJob, config)
	return startingStatus, nil
}

func (m *Manager) Stop() (Status, error) {
	if m == nil {
		return Status{}, ErrNoActiveJob
	}
	m.mu.Lock()
	current := m.current
	if current == nil || !current.status.Active {
		m.mu.Unlock()
		return Status{}, ErrNoActiveJob
	}
	current.status.Phase = "cancelling"
	status := cloneStatus(current.status)
	current.cancel()
	m.mu.Unlock()
	return status, nil
}

func (m *Manager) Status() Status {
	if m == nil {
		return Status{Enabled: false, Phase: "unavailable"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return Status{Enabled: true, PoseTrainingEnabled: false, SupportedTasks: []string{"activity_classifier"}, Phase: "idle"}
	}
	return cloneStatus(m.current.status)
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	current := m.current
	if current != nil && current.status.Active {
		current.cancel()
	}
	m.mu.Unlock()
	if current != nil {
		<-current.done
	}
}

func (m *Manager) run(ctx context.Context, current *job, config Config) {
	defer close(current.done)
	setPhase := func(phase string) {
		m.mu.Lock()
		if m.current == current {
			current.status.Phase = phase
		}
		m.mu.Unlock()
	}
	finish := func(phase, message string) {
		m.mu.Lock()
		if m.current == current {
			current.status.Active = false
			current.status.Phase = phase
			current.status.Error = message
			current.status.FinishedAt = time.Now().UTC()
		}
		m.mu.Unlock()
	}
	setPhase("training")
	trained, validation, err := adaptive.TrainValidatedFiles(ctx, m.recordingsDir, current.status.TrainingDatasetIDs, current.status.ValidationDatasetIDs, adaptive.TrainingOptions{
		Epochs: config.Epochs, BatchSize: config.BatchSize, LearningRate: config.LearningRate,
		WeightDecay: config.WeightDecay, Patience: config.Patience,
	}, func(progress adaptive.TrainingProgress) {
		m.mu.Lock()
		if m.current == current {
			current.status.Epoch = progress.Epoch
			current.status.TotalEpochs = progress.TotalEpochs
			if progress.ValidationAccuracy != nil {
				value := *progress.ValidationAccuracy
				current.status.ValidationAccuracy = &value
			}
		}
		m.mu.Unlock()
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			finish("cancelled", "training was cancelled")
		} else {
			finish("failed", err.Error())
		}
		return
	}
	if err := ctx.Err(); err != nil {
		finish("cancelled", "training was cancelled")
		return
	}
	m.mu.Lock()
	if m.current == current {
		current.status.TrainSamples = trained.TrainedFrames
		accuracy := trained.TrainingAccuracy
		current.status.TrainAccuracy = &accuracy
	}
	m.mu.Unlock()
	if trained.ValidationSamples == 0 || validation.Samples != trained.ValidationSamples {
		finish("failed", "held-out validation metrics were not produced")
		return
	}
	setPhase("exporting")
	modelID := fmt.Sprintf("trained-activity-%s-%d", time.Now().UTC().Format("20060102_150405_000000000"), m.sequence.Add(1))
	artifact, err := model.BuildActivityRVF(modelID, trained)
	m.mu.Lock()
	if m.current == current {
		if cancelErr := ctx.Err(); cancelErr != nil {
			current.status.Active = false
			current.status.Phase = "cancelled"
			current.status.Error = "training was cancelled"
			current.status.FinishedAt = time.Now().UTC()
			m.mu.Unlock()
			return
		}
		if err == nil {
			err = m.models.InstallGenerated(modelID, artifact)
		}
		if err != nil {
			current.status.Active = false
			current.status.Phase = "failed"
			current.status.Error = err.Error()
			current.status.FinishedAt = time.Now().UTC()
			m.mu.Unlock()
			return
		}
		accuracy := validation.Accuracy
		current.status.ValidationAccuracy = &accuracy
		current.status.ValidationSamples = validation.Samples
		current.status.ValidationSplit = trained.ValidationSplit
		current.status.ModelID = modelID
		current.status.Active = false
		current.status.Phase = "completed"
		current.status.FinishedAt = time.Now().UTC()
	}
	m.mu.Unlock()
}

func splitDatasets(ids []string, lookup func(string) (string, bool, error)) ([]string, []string, error) {
	if len(ids) == 0 {
		return nil, nil, errors.New("dataset_ids must include at least two labelled recordings per class")
	}
	if len(ids) > 128 {
		return nil, nil, errors.New("selected datasets exceed the 128-file limit")
	}
	selected := make(map[string]string, len(ids))
	byClass := make(map[string][]string)
	for _, id := range ids {
		if strings.TrimSpace(id) != id || id == "" {
			return nil, nil, errors.New("dataset ids must be non-empty exact IDs")
		}
		if _, exists := selected[id]; exists {
			return nil, nil, fmt.Errorf("dataset %q was selected more than once", id)
		}
		label, eligible, err := lookup(id)
		if err != nil {
			return nil, nil, fmt.Errorf("dataset %q: %w", id, err)
		}
		if !eligible {
			return nil, nil, fmt.Errorf("dataset %q is not eligible for training", id)
		}
		selected[id] = label
		byClass[label] = append(byClass[label], id)
	}
	if len(byClass) < 2 {
		return nil, nil, errors.New("activity training requires at least two classes")
	}
	classes := make([]string, 0, len(byClass))
	for class := range byClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	trainingIDs := make([]string, 0, len(ids))
	validationIDs := make([]string, 0, len(classes))
	for _, class := range classes {
		classIDs := byClass[class]
		sort.Strings(classIDs)
		if len(classIDs) < 2 {
			return nil, nil, fmt.Errorf("class %q needs at least two distinct recording files for a held-out check", class)
		}
		validationIDs = append(validationIDs, classIDs[len(classIDs)-1])
		trainingIDs = append(trainingIDs, classIDs[:len(classIDs)-1]...)
	}
	return trainingIDs, validationIDs, nil
}

func cloneStatus(status Status) Status {
	status.SupportedTasks = append([]string(nil), status.SupportedTasks...)
	status.DatasetIDs = append([]string(nil), status.DatasetIDs...)
	status.TrainingDatasetIDs = append([]string(nil), status.TrainingDatasetIDs...)
	status.ValidationDatasetIDs = append([]string(nil), status.ValidationDatasetIDs...)
	if status.TrainAccuracy != nil {
		value := *status.TrainAccuracy
		status.TrainAccuracy = &value
	}
	if status.ValidationAccuracy != nil {
		value := *status.ValidationAccuracy
		status.ValidationAccuracy = &value
	}
	return status
}
