package localize

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxMetricAnchors = 64
	minMetricAnchors = 6
	maxMetricRMSE    = 0.50
	maxMetricError   = 1.00
)

var ErrMetricCalibration = errors.New("invalid metric calibration")

type MetricAnchor struct {
	Image [2]float64 `json:"image_point"`
	Room  [2]float64 `json:"room_floor_m"`
}

type MetricCalibrationStatus struct {
	Available                   bool     `json:"available"`
	Reason                      string   `json:"reason"`
	CoordinateFrame             string   `json:"coordinate_frame"`
	ModelSHA256                 string   `json:"model_sha256,omitempty"`
	AnchorCount                 int      `json:"anchor_count"`
	Validation                  string   `json:"validation_method"`
	LeaveOneOutRMSE             *float64 `json:"leave_one_out_rmse_m,omitempty"`
	LeaveOneOutMaxError         *float64 `json:"leave_one_out_max_error_m,omitempty"`
	Saved                       bool     `json:"saved"`
	PoseModelQualityValidated   bool     `json:"pose_model_quality_validated"`
	PersonLocalizationAvailable bool     `json:"person_localization_available"`
}

type metricCalibrationFile struct {
	Version     int        `json:"version"`
	ModelSHA256 string     `json:"model_sha256"`
	Transform   [8]float64 `json:"pose_output_to_room_homography"`
	AnchorCount int        `json:"anchor_count"`
	Validation  string     `json:"validation_method"`
	LOORMSE     float64    `json:"leave_one_out_rmse_m"`
	LOOMaxError float64    `json:"leave_one_out_max_error_m"`
	CreatedAt   time.Time  `json:"created_at"`
}

// MetricCalibration maps normalized image-plane ankle estimates onto a
// calibrated room-floor coordinate frame. It validates only the coordinate
// transform; it cannot validate the pose model that produced the image point.
type MetricCalibration struct {
	mu        sync.RWMutex
	path      string
	modelHash string
	anchors   []MetricAnchor
	state     metricCalibrationFile
	available bool
	reason    string
}

func NewMetricCalibration(path, modelHash string) (*MetricCalibration, error) {
	m := &MetricCalibration{path: path, modelHash: modelHash, reason: "pose_model_not_loaded"}
	if modelHash == "" {
		return m, nil
	}
	if path == "" {
		m.reason = "calibration_storage_not_configured"
		return m, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		m.reason = "collect_at_least_six_ground_truth_anchors"
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat metric calibration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return nil, fmt.Errorf("%w: calibration must be a regular file no larger than 16 KiB", ErrMetricCalibration)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metric calibration: %w", err)
	}
	var saved metricCalibrationFile
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode metric calibration: %w", err)
	}
	if err := validateSavedCalibration(saved, modelHash); err != nil {
		return nil, err
	}
	m.state, m.available, m.reason = saved, true, "calibration_transform_ready_pose_quality_unvalidated"
	return m, nil
}

func (m *MetricCalibration) AddAnchor(anchor MetricAnchor) error {
	if m == nil {
		return errors.New("metric calibration is unavailable")
	}
	if err := validateMetricAnchor(anchor); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelHash == "" || m.path == "" {
		return errors.New("pose model and calibration storage must be configured")
	}
	if len(m.anchors) >= maxMetricAnchors {
		return fmt.Errorf("maximum of %d in-memory calibration anchors reached", maxMetricAnchors)
	}
	for _, existing := range m.anchors {
		if math.Hypot(existing.Image[0]-anchor.Image[0], existing.Image[1]-anchor.Image[1]) < 1e-4 {
			return fmt.Errorf("%w: image anchor duplicates an existing point", ErrMetricCalibration)
		}
		if math.Hypot(existing.Room[0]-anchor.Room[0], existing.Room[1]-anchor.Room[1]) < 1e-3 {
			return fmt.Errorf("%w: room anchor duplicates an existing point", ErrMetricCalibration)
		}
	}
	if len(m.anchors) == 0 {
		if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove previous metric calibration: %w", err)
		}
		m.available = false
		m.state = metricCalibrationFile{}
	}
	m.anchors = append(m.anchors, anchor)
	m.reason = "collect_at_least_six_ground_truth_anchors"
	return nil
}

func (m *MetricCalibration) Fit() error {
	if m == nil {
		return errors.New("metric calibration is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelHash == "" || m.path == "" {
		return errors.New("pose model and calibration storage must be configured")
	}
	if len(m.anchors) < minMetricAnchors {
		return fmt.Errorf("at least %d distinct ground-truth anchors are required", minMetricAnchors)
	}
	transform, err := fitHomography(m.anchors)
	if err != nil {
		return err
	}
	var squaredError float64
	var maxError float64
	for omitted := range m.anchors {
		training := make([]MetricAnchor, 0, len(m.anchors)-1)
		training = append(training, m.anchors[:omitted]...)
		training = append(training, m.anchors[omitted+1:]...)
		fold, fitErr := fitHomography(training)
		if fitErr != nil {
			return fmt.Errorf("leave-one-out validation failed: %w", fitErr)
		}
		predicted, mapErr := applyHomography(fold, m.anchors[omitted].Image)
		if mapErr != nil {
			return fmt.Errorf("leave-one-out validation failed: %w", mapErr)
		}
		distance := math.Hypot(predicted[0]-m.anchors[omitted].Room[0], predicted[1]-m.anchors[omitted].Room[1])
		squaredError += distance * distance
		if distance > maxError {
			maxError = distance
		}
	}
	rmse := math.Sqrt(squaredError / float64(len(m.anchors)))
	if rmse > maxMetricRMSE || maxError > maxMetricError {
		return fmt.Errorf("%w: leave-one-out errors exceed limits (RMSE %.3f m, max %.3f m)", ErrMetricCalibration, rmse, maxError)
	}
	saved := metricCalibrationFile{
		Version: 1, ModelSHA256: m.modelHash, Transform: transform,
		AnchorCount: len(m.anchors), Validation: "leave_one_anchor_out",
		LOORMSE: rmse, LOOMaxError: maxError, CreatedAt: time.Now().UTC(),
	}
	if err := validateSavedCalibration(saved, m.modelHash); err != nil {
		return err
	}
	if err := writeCalibrationAtomically(m.path, saved); err != nil {
		return err
	}
	m.state, m.available = saved, true
	m.reason = "calibration_transform_ready_pose_quality_unvalidated"
	return nil
}

func (m *MetricCalibration) Map(imagePoint [2]float64) ([2]float64, error) {
	if m == nil {
		return [2]float64{}, errors.New("metric calibration is unavailable")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.available {
		return [2]float64{}, errors.New(m.reason)
	}
	if imagePoint[0] < 0 || imagePoint[0] > 1 || imagePoint[1] < 0 || imagePoint[1] > 1 {
		return [2]float64{}, fmt.Errorf("%w: image coordinate must be normalized to [0,1]", ErrMetricCalibration)
	}
	return applyHomography(m.state.Transform, imagePoint)
}

func (m *MetricCalibration) Status() MetricCalibrationStatus {
	if m == nil {
		return MetricCalibrationStatus{Reason: "calibration_unavailable", CoordinateFrame: "room_floor_m", Validation: "leave_one_anchor_out"}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := MetricCalibrationStatus{
		Available: m.available, Reason: m.reason, CoordinateFrame: "room_floor_m",
		ModelSHA256: m.modelHash, AnchorCount: len(m.anchors), Validation: "leave_one_anchor_out",
		PoseModelQualityValidated: false, PersonLocalizationAvailable: false,
	}
	if m.available {
		status.AnchorCount = m.state.AnchorCount
		rmse, maxError := m.state.LOORMSE, m.state.LOOMaxError
		status.LeaveOneOutRMSE, status.LeaveOneOutMaxError = &rmse, &maxError
		status.Saved = true
	}
	return status
}

func (m *MetricCalibration) Reset() error {
	if m == nil {
		return errors.New("metric calibration is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path != "" {
		if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove metric calibration: %w", err)
		}
	}
	m.anchors = nil
	m.state = metricCalibrationFile{}
	m.available = false
	if m.modelHash == "" {
		m.reason = "pose_model_not_loaded"
	} else {
		m.reason = "collect_at_least_six_ground_truth_anchors"
	}
	return nil
}

func validateMetricAnchor(anchor MetricAnchor) error {
	for _, value := range anchor.Image {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fmt.Errorf("%w: image coordinates must be finite and in [0,1]", ErrMetricCalibration)
		}
	}
	for _, value := range anchor.Room {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1000 {
			return fmt.Errorf("%w: room coordinates must be finite and within 1000 meters", ErrMetricCalibration)
		}
	}
	return nil
}

func validateSavedCalibration(saved metricCalibrationFile, modelHash string) error {
	if saved.Version != 1 || saved.ModelSHA256 != modelHash || saved.AnchorCount < minMetricAnchors ||
		saved.Validation != "leave_one_anchor_out" || !finite(saved.LOORMSE) || !finite(saved.LOOMaxError) ||
		saved.LOORMSE > maxMetricRMSE || saved.LOOMaxError > maxMetricError || saved.CreatedAt.IsZero() {
		return fmt.Errorf("%w: saved calibration metadata is unsupported or does not match the pose model", ErrMetricCalibration)
	}
	for _, value := range saved.Transform {
		if !finite(value) || math.Abs(value) > 1e6 {
			return fmt.Errorf("%w: saved homography contains an invalid coefficient", ErrMetricCalibration)
		}
	}
	return nil
}

func writeCalibrationAtomically(path string, saved metricCalibrationFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create calibration directory: %w", err)
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("encode metric calibration: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pose-calibration-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary metric calibration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict metric calibration permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write metric calibration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync metric calibration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close metric calibration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install metric calibration: %w", err)
	}
	return nil
}

func fitHomography(anchors []MetricAnchor) ([8]float64, error) {
	if len(anchors) < 4 {
		return [8]float64{}, fmt.Errorf("%w: at least four anchors are required to fit a homography", ErrMetricCalibration)
	}
	var normal [8][8]float64
	var rhs [8]float64
	for _, anchor := range anchors {
		x, y := anchor.Image[0], anchor.Image[1]
		u, v := anchor.Room[0], anchor.Room[1]
		rows := [2][8]float64{
			{x, y, 1, 0, 0, 0, -x * u, -y * u},
			{0, 0, 0, x, y, 1, -x * v, -y * v},
		}
		values := [2]float64{u, v}
		for i, row := range rows {
			for r := 0; r < 8; r++ {
				rhs[r] += row[r] * values[i]
				for c := 0; c < 8; c++ {
					normal[r][c] += row[r] * row[c]
				}
			}
		}
	}
	return solve8(normal, rhs)
}

func solve8(matrix [8][8]float64, rhs [8]float64) ([8]float64, error) {
	for column := 0; column < 8; column++ {
		pivot := column
		for row := column + 1; row < 8; row++ {
			if math.Abs(matrix[row][column]) > math.Abs(matrix[pivot][column]) {
				pivot = row
			}
		}
		if math.Abs(matrix[pivot][column]) < 1e-12 {
			return [8]float64{}, fmt.Errorf("%w: calibration anchors are collinear or ill-conditioned", ErrMetricCalibration)
		}
		matrix[column], matrix[pivot] = matrix[pivot], matrix[column]
		rhs[column], rhs[pivot] = rhs[pivot], rhs[column]
		divisor := matrix[column][column]
		for c := column; c < 8; c++ {
			matrix[column][c] /= divisor
		}
		rhs[column] /= divisor
		for row := 0; row < 8; row++ {
			if row == column {
				continue
			}
			factor := matrix[row][column]
			for c := column; c < 8; c++ {
				matrix[row][c] -= factor * matrix[column][c]
			}
			rhs[row] -= factor * rhs[column]
		}
	}
	return rhs, nil
}

func applyHomography(transform [8]float64, image [2]float64) ([2]float64, error) {
	x, y := image[0], image[1]
	denominator := transform[6]*x + transform[7]*y + 1
	if !finite(denominator) || math.Abs(denominator) < 1e-9 {
		return [2]float64{}, fmt.Errorf("%w: image point maps to a singular homography", ErrMetricCalibration)
	}
	room := [2]float64{
		(transform[0]*x + transform[1]*y + transform[2]) / denominator,
		(transform[3]*x + transform[4]*y + transform[5]) / denominator,
	}
	if !finite(room[0]) || !finite(room[1]) || math.Abs(room[0]) > 1000 || math.Abs(room[1]) > 1000 {
		return [2]float64{}, fmt.Errorf("%w: mapped coordinate is outside supported bounds", ErrMetricCalibration)
	}
	return room, nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
