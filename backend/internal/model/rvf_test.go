package model

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/signal"
)

func testRVF(t *testing.T, weights []float32) []byte {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{"model_id": "test", "version": "1.0.0", "description": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	weightData := make([]byte, len(weights)*4)
	for index, weight := range weights {
		binary.LittleEndian.PutUint32(weightData[index*4:index*4+4], math.Float32bits(weight))
	}
	return BuildRVF(
		Segment{Type: segmentManifest, Payload: manifest},
		Segment{Type: segmentVector, Payload: weightData},
		Segment{Type: segmentLoRA, Payload: append([]byte{7, 0}, append([]byte("default"), weightData...)...)},
	)
}

func TestRVFParseAndValidateContentHash(t *testing.T) {
	data := testRVF(t, []float32{1.25, -0.5})
	container, err := ParseRVF(data)
	if err != nil {
		t.Fatal(err)
	}
	weights, err := container.Weights()
	if err != nil || len(weights) != 2 || weights[0] != 1.25 || weights[1] != -0.5 {
		t.Fatalf("weights=%v err=%v", weights, err)
	}
	if err := container.ValidateWeights(); err != nil || container.WeightCount() != 2 {
		t.Fatalf("weight validation/count failed: count=%d err=%v", container.WeightCount(), err)
	}
	if got := container.Manifest()["version"]; got != "1.0.0" {
		t.Fatalf("manifest version=%v", got)
	}
	if container.Segments[0].ID != 0 || container.Segments[1].ID != 1 || container.Segments[2].ID != 2 {
		t.Fatalf("default segment ids=%v, want Rust-compatible ids [0 1 2]", []uint64{
			container.Segments[0].ID, container.Segments[1].ID, container.Segments[2].ID,
		})
	}
	if profiles := container.LoRAProfiles(); len(profiles) != 1 || profiles[0] != "default" {
		t.Fatalf("LoRA profiles=%v", profiles)
	}
	data[64] ^= 1
	if _, err := ParseRVF(data); err == nil {
		t.Fatal("corrupt segment content was accepted")
	}
}

func TestBuildRVFPreservesExplicitSegmentIDs(t *testing.T) {
	container, err := ParseRVF(BuildRVF(Segment{ID: 42, Type: segmentManifest, Payload: []byte(`{"version":"1.0"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	if container.Segments[0].ID != 42 {
		t.Fatalf("segment id=%d, want preserved ID 42", container.Segments[0].ID)
	}
}

func TestRVFRejectsTruncationAndNonFiniteWeights(t *testing.T) {
	data := testRVF(t, []float32{float32(math.NaN())})
	container, err := ParseRVF(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := container.Weights(); err == nil {
		t.Fatal("NaN model weight was accepted")
	}
	if err := container.ValidateWeights(); err == nil {
		t.Fatal("NaN model weight passed allocation-free validation")
	}
	if _, err := ParseRVF(data[:len(data)-1]); err == nil {
		t.Fatal("truncated RVF container was accepted")
	}
}

func TestRegistryModelLifecycleAndPathSafety(t *testing.T) {
	dir := t.TempDir()
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.rvf"), testRVF(t, []float32{1, 2, 3}), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := registry.List()
	if err != nil || len(models) != 1 || !models[0].Loadable {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if _, err := registry.Get("../fixture"); err != ErrInvalidID {
		t.Fatalf("path traversal error=%v", err)
	}
	active, err := registry.Load("fixture")
	if err != nil || active.WeightCount != 3 || active.InferenceAvailable {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if err := registry.ActivateLoRA("fixture", "default"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete("fixture"); err == nil {
		t.Fatal("active model was deleted")
	}
	if _, err := registry.Unload(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete("fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestActivityRVFWeightsDriveExecutableInference(t *testing.T) {
	weights := [][]float64{make([]float64, adaptive.FeatureCount+1), make([]float64, adaptive.FeatureCount+1)}
	weights[0][adaptive.FeatureCount] = -8
	weights[1][adaptive.FeatureCount] = 8
	std := [adaptive.FeatureCount]float64{}
	for i := range std {
		std[i] = 1
	}
	trained := &adaptive.Model{
		ClassNames: []string{"absent", "present_moving"},
		ClassStats: []adaptive.ClassStats{{Label: "absent", Count: 3}, {Label: "present_moving", Count: 3}},
		GlobalStd:  std, Weights: weights, TrainedFrames: 6,
		TrainingAccuracy: 1, ValidationAccuracy: 0.75, ValidationSamples: 8,
		ValidationSplit: "recording_file_holdout", Version: adaptive.TrainedModelVersion,
	}
	data, err := BuildActivityRVF("activity", trained)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.InstallGenerated("activity", data); err != nil {
		t.Fatal(err)
	}
	active, err := registry.Load("activity")
	if err != nil || !active.InferenceAvailable || active.Architecture != ActivityArchitecture {
		t.Fatalf("loaded model did not expose its executable architecture: active=%+v err=%v", active, err)
	}
	observation := &signal.Observation{
		Class:     signal.Classification{MotionLevel: "absent", Confidence: 0.1},
		Amplitude: []float64{1, 2, 3},
	}
	used, err := registry.ApplyObservation(observation)
	if err != nil || !used || observation.Class.MotionLevel != "present_moving" || !observation.Class.Presence {
		t.Fatalf("RVF weights did not affect the prediction: used=%v class=%+v err=%v", used, observation.Class, err)
	}
	active, ok := registry.Active()
	if !ok || active.FramesProcessed != 1 || active.ValidationAccuracy == nil || *active.ValidationAccuracy != 0.75 || active.ValidationSamples != 8 {
		t.Fatalf("active inference statistics/validation metadata missing: %+v", active)
	}
}

func TestMalformedSupportedActivityContractCannotBeLoaded(t *testing.T) {
	manifest, err := json.Marshal(map[string]any{
		"model_id": "bad", "inference": map[string]any{
			"architecture": ActivityArchitecture, "feature_schema": ActivityFeatureSchema,
			"feature_count": adaptive.FeatureCount, "weight_layout": "classes_x_16_row_major",
			"class_names": []string{"absent", "moving"}, "class_stats": []adaptive.ClassStats{},
			"trained_frames": 2, "global_std": make([]float64, adaptive.FeatureCount),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data := BuildRVF(
		Segment{Type: segmentManifest, Payload: manifest},
		Segment{Type: segmentVector, Payload: make([]byte, 2*(adaptive.FeatureCount+1)*4)},
	)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.rvf"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Load("bad"); err == nil {
		t.Fatal("malformed declared architecture was silently loaded as a non-inference model")
	}
}
