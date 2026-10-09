package localize

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestMetricCalibrationFitsAndPersistsHomography(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pose_metric.json")
	mapper, err := NewMetricCalibration(path, "model-hash")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range [][2]float64{{0.1, 0.1}, {0.9, 0.1}, {0.1, 0.9}, {0.9, 0.9}, {0.5, 0.2}, {0.3, 0.6}} {
		room := [2]float64{1 + 4*image[0], 2 + 3*image[1]}
		if err := mapper.AddAnchor(MetricAnchor{Image: image, Room: room}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mapper.Fit(); err != nil {
		t.Fatal(err)
	}
	got, err := mapper.Map([2]float64{0.25, 0.75})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got[0]-2) > 1e-6 || math.Abs(got[1]-4.25) > 1e-6 {
		t.Fatalf("unexpected mapped room coordinate: %v", got)
	}
	status := mapper.Status()
	if !status.Available || status.PersonLocalizationAvailable || status.PoseModelQualityValidated || status.AnchorCount != 6 {
		t.Fatalf("calibration transform must not imply validated person localization: %+v", status)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("calibration permissions = %o, want 600", info.Mode().Perm())
	}

	reloaded, err := NewMetricCalibration(path, "model-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Status().Available {
		t.Fatalf("valid calibration did not reload: %+v", reloaded.Status())
	}
	if _, err := NewMetricCalibration(path, "different-model-hash"); err == nil {
		t.Fatal("calibration for a different pose model was accepted")
	}
}

func TestMetricCalibrationRejectsInsufficientDuplicateAndCollinearAnchors(t *testing.T) {
	mapper, err := NewMetricCalibration(filepath.Join(t.TempDir(), "pose_metric.json"), "model-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := mapper.Fit(); err == nil {
		t.Fatal("fit without anchors succeeded")
	}
	if err := mapper.AddAnchor(MetricAnchor{Image: [2]float64{0.2, 0.2}, Room: [2]float64{1, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := mapper.AddAnchor(MetricAnchor{Image: [2]float64{0.2, 0.2}, Room: [2]float64{2, 2}}); err == nil {
		t.Fatal("duplicate image anchor was accepted")
	}
	if err := mapper.Reset(); err != nil {
		t.Fatal(err)
	}

	for i, image := range [][2]float64{{0.1, 0.2}, {0.2, 0.2}, {0.3, 0.2}, {0.4, 0.2}, {0.5, 0.2}, {0.7, 0.2}} {
		if err := mapper.AddAnchor(MetricAnchor{Image: image, Room: [2]float64{float64(i + 1), 2}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mapper.Fit(); err == nil {
		t.Fatal("collinear anchors produced an accepted calibration")
	}
	if _, err := mapper.Map([2]float64{0.3, 0.2}); err == nil {
		t.Fatal("failed calibration became available")
	}
}

func TestMetricCalibrationRejectsPoorLeaveOneOutGeneralization(t *testing.T) {
	mapper, err := NewMetricCalibration(filepath.Join(t.TempDir(), "pose_metric.json"), "model-hash")
	if err != nil {
		t.Fatal(err)
	}
	images := [][2]float64{{0.1, 0.1}, {0.9, 0.1}, {0.1, 0.9}, {0.9, 0.9}, {0.5, 0.2}, {0.3, 0.6}}
	for i, image := range images {
		room := [2]float64{1 + 4*image[0], 2 + 3*image[1]}
		if i == len(images)-1 {
			room[0] += 4
		}
		if err := mapper.AddAnchor(MetricAnchor{Image: image, Room: room}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mapper.Fit(); err == nil {
		t.Fatal("poorly generalizing calibration passed leave-one-out gates")
	}
	if mapper.Status().Available {
		t.Fatal("failed validation enabled metric candidate mapping")
	}
}

func TestMetricCalibrationRejectsInvalidCoordinatesAndResetRemovesSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pose_metric.json")
	mapper, err := NewMetricCalibration(path, "model-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := mapper.AddAnchor(MetricAnchor{Image: [2]float64{math.NaN(), 0.5}, Room: [2]float64{1, 1}}); err == nil {
		t.Fatal("non-finite image coordinate was accepted")
	}
	for _, image := range [][2]float64{{0.1, 0.1}, {0.9, 0.1}, {0.1, 0.9}, {0.9, 0.9}, {0.5, 0.2}, {0.3, 0.6}} {
		if err := mapper.AddAnchor(MetricAnchor{Image: image, Room: [2]float64{1 + 4*image[0], 2 + 3*image[1]}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mapper.Fit(); err != nil {
		t.Fatal(err)
	}
	if err := mapper.Reset(); err != nil {
		t.Fatal(err)
	}
	if mapper.Status().Available {
		t.Fatal("reset left metric mapping available")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("calibration file remains after reset: err=%v", err)
	}
}
