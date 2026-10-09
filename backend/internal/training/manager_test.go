package training

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/model"
)

func writeLabeledRecording(t *testing.T, root, name string, motion float64, amplitude []float64) {
	t.Helper()
	file, err := os.Create(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for frame := 0; frame < 12; frame++ {
		value := map[string]any{
			"features": map[string]any{
				"variance": motion, "motion_band_power": motion * 2,
				"breathing_band_power": 0.1, "spectral_power": motion + 1,
				"dominant_freq_hz": 0.25, "change_points": int(motion), "mean_rssi": -55,
			},
			"nodes": []any{map[string]any{"node_id": 1, "amplitude": amplitude}},
		}
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSplitDatasetsIsFileDisjointAndRequiresEveryClassHoldout(t *testing.T) {
	labels := map[string]string{
		"train_absent_a": "absent", "train_absent_b": "absent",
		"train_walking_a": "present_moving", "train_walking_b": "present_moving",
	}
	train, validation, err := splitDatasets([]string{"train_absent_a", "train_absent_b", "train_walking_a", "train_walking_b"}, func(id string) (string, bool, error) {
		label, ok := labels[id]
		if !ok {
			return "", false, os.ErrNotExist
		}
		return label, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(train) != 2 || len(validation) != 2 || train[0] == validation[0] || train[1] == validation[1] {
		t.Fatalf("unexpected file-disjoint split: train=%v validation=%v", train, validation)
	}
	if _, _, err := splitDatasets([]string{"train_absent_a", "train_walking_a"}, func(id string) (string, bool, error) {
		return labels[id], true, nil
	}); err == nil {
		t.Fatal("single file per class was accepted without a held-out recording")
	}
}

func TestManagerTrainsExportsAndLoadsExecutableRVF(t *testing.T) {
	dataDir := t.TempDir()
	recordingsDir := filepath.Join(dataDir, "recordings")
	if err := os.MkdirAll(recordingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLabeledRecording(t, recordingsDir, "train_absent_room_a.jsonl", 0.1, []float64{1, 1, 1, 1})
	writeLabeledRecording(t, recordingsDir, "train_absent_room_b.jsonl", 0.2, []float64{1, 2, 1, 2})
	writeLabeledRecording(t, recordingsDir, "train_walking_room_a.jsonl", 10, []float64{2, 6, 4, 9})
	writeLabeledRecording(t, recordingsDir, "train_walking_room_b.jsonl", 11, []float64{3, 7, 5, 10})
	models, err := model.NewRegistry(filepath.Join(dataDir, "models"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(dataDir, models)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	datasets, err := manager.Datasets()
	if err != nil || len(datasets) != 4 {
		t.Fatalf("datasets=%+v err=%v", datasets, err)
	}
	ids := []string{datasets[0].ID, datasets[1].ID, datasets[2].ID, datasets[3].ID}
	started, err := manager.Start(ids, Config{Epochs: 5})
	if err != nil || !started.Active || started.Task != "activity_classifier" || len(started.ValidationDatasetIDs) != 2 {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for manager.Status().Active && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	manager.Close()
	status := manager.Status()
	if status.Active || status.Phase != "completed" || status.ModelID == "" || status.ValidationAccuracy == nil || status.ValidationSamples == 0 || status.ValidationSplit != "recording_file_holdout" {
		t.Fatalf("training did not finish with held-out metrics: %+v", status)
	}
	active, err := models.Load(status.ModelID)
	if err != nil || !active.InferenceAvailable || active.Architecture != model.ActivityArchitecture {
		t.Fatalf("trained RVF artifact was not executable: active=%+v err=%v", active, err)
	}
}

func TestManagerStopCancelsActiveJob(t *testing.T) {
	dataDir := t.TempDir()
	recordingsDir := filepath.Join(dataDir, "recordings")
	if err := os.MkdirAll(recordingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLabeledRecording(t, recordingsDir, "train_absent_a.jsonl", 0.1, []float64{1, 1, 1, 1})
	writeLabeledRecording(t, recordingsDir, "train_absent_b.jsonl", 0.2, []float64{1, 2, 1, 2})
	writeLabeledRecording(t, recordingsDir, "train_walking_a.jsonl", 10, []float64{2, 6, 4, 9})
	writeLabeledRecording(t, recordingsDir, "train_walking_b.jsonl", 11, []float64{3, 7, 5, 10})
	models, err := model.NewRegistry(filepath.Join(dataDir, "models"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(dataDir, models)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	datasets, err := manager.Datasets()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{datasets[0].ID, datasets[1].ID, datasets[2].ID, datasets[3].ID}
	if _, err := manager.Start(ids, Config{Epochs: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	if status := manager.Status(); status.Active || status.Phase != "cancelled" {
		t.Fatalf("training stop did not cancel job: %+v", status)
	}
}

func TestManagerRejectsMissingAndDuplicateDatasets(t *testing.T) {
	dataDir := t.TempDir()
	models, err := model.NewRegistry(filepath.Join(dataDir, "models"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(dataDir, models)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := manager.Start(nil, Config{Epochs: 10}); err == nil {
		t.Fatal("empty dataset selection was accepted")
	}
	if _, err := manager.Start([]string{"../escape"}, Config{Epochs: 10}); err == nil {
		t.Fatal("path-like dataset id was accepted")
	}
}
