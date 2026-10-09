package adaptive

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ruvnet/ruview/backend/internal/signal"
)

func testModel() *Model {
	weights := [][]float64{make([]float64, FeatureCount+1), make([]float64, FeatureCount+1)}
	weights[0][FeatureCount] = -2
	weights[1][FeatureCount] = 2
	return &Model{
		ClassStats: []ClassStats{
			{Label: "absent", Count: 4},
			{Label: "present_moving", Count: 4},
		},
		Weights: weights, GlobalStd: [FeatureCount]float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		TrainedFrames: 8, TrainingAccuracy: 0.875, Version: TrainedModelVersion,
		ClassNames: []string{"absent", "present_moving"},
	}
}

func TestModelValidationClassificationAndClone(t *testing.T) {
	model := testModel()
	label, confidence, err := model.Classify([FeatureCount]float64{})
	if err != nil || label != "present_moving" || confidence < 0.95 {
		t.Fatalf("classification label=%q confidence=%f err=%v", label, confidence, err)
	}
	observation := signal.Observation{
		Features:  signal.FeatureInfo{Variance: 0.3},
		Class:     signal.Classification{MotionLevel: "absent", Confidence: 0.5},
		Amplitude: []float64{1, 2, 3},
	}
	if err := model.Apply(&observation); err != nil {
		t.Fatal(err)
	}
	if observation.Class.MotionLevel != "present_moving" || !observation.Class.Presence || observation.Class.Confidence <= 0.5 {
		t.Fatalf("adaptive override was not applied: %+v", observation.Class)
	}
	clone := model.Clone()
	clone.Weights[1][FeatureCount] = -100
	if model.Weights[1][FeatureCount] != 2 {
		t.Fatal("model clone shares mutable weights")
	}
	if _, _, err := model.Classify([FeatureCount]float64{math.NaN()}); err == nil {
		t.Fatal("non-finite feature was accepted")
	}
}

func TestModelSaveLoadIsAtomicAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adaptive_model.json")
	model := testModel()
	if err := model.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(model, loaded) {
		t.Fatalf("loaded model differs: loaded=%+v err=%v", loaded, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("adaptive model permissions=%o, want 600", info.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte(`{"class_names":["x"],"class_stats":[],"weights":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("dimensionally invalid model was accepted")
	}
}

func TestTrainingFromLabeledRecordingsIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeRecording := func(name string, variance float64, amplitude []float64) {
		t.Helper()
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for i := 0; i < 8; i++ {
			frame := map[string]any{
				"features": map[string]any{
					"variance": variance, "motion_band_power": variance * 2,
					"breathing_band_power": 0.1, "spectral_power": variance + 1,
					"dominant_freq_hz": 0.25, "change_points": 2, "mean_rssi": -55,
				},
				"nodes": []any{map[string]any{"node_id": 1, "amplitude": amplitude}},
			}
			if err := encoder.Encode(frame); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	writeRecording("train_absent_emptyroom.jsonl", 0.1, []float64{1, 1, 1, 1})
	writeRecording("train_walking_hallway.jsonl", 10, []float64{2, 6, 4, 9})

	first, err := Train(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Train(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.TrainedFrames != 16 || first.ClassNames[0] != "absent" || first.ClassNames[1] != "present_moving" {
		t.Fatalf("unexpected trained model summary: %+v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same recordings produced a non-deterministic model")
	}
	if first.TrainingAccuracy < 0.99 {
		t.Fatalf("simple separable fixture not learned: in-sample accuracy=%f", first.TrainingAccuracy)
	}
}

func TestTrainingRejectsMissingOrOversizedInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := Train(context.Background(), dir); err == nil {
		t.Fatal("training without labeled recordings succeeded")
	}
	file, err := os.Create(filepath.Join(dir, "train_still_big.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxTrainingFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Train(context.Background(), dir); err == nil {
		t.Fatal("oversized training file was accepted")
	}
	if err := os.Remove(filepath.Join(dir, "train_still_big.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train_absent_test.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Train(canceled, dir); err != context.Canceled {
		t.Fatalf("cancelled training error=%v, want context.Canceled", err)
	}
}

func TestNodeRecordingSamplerUsesFreshChangedNodeFrames(t *testing.T) {
	var last map[uint64][]float64 = make(map[uint64][]float64)
	line := []byte(`{"nodes":[{"node_id":1,"amplitude":[1,2]},{"node_id":2,"amplitude":[3,4]}],"node_features":[{"node_id":1,"stale":false,"features":{"variance":10}},{"node_id":2,"stale":true,"features":{"variance":20}}]}`)
	first, ok := samplesFromLine(line, last)
	if !ok || len(first) != 1 || first[0][0] != 10 {
		t.Fatalf("first node samples=%v ok=%v", first, ok)
	}
	second, ok := samplesFromLine(line, last)
	if !ok || len(second) != 0 {
		t.Fatalf("unchanged amplitude snapshot was resampled: %v ok=%v", second, ok)
	}
}
