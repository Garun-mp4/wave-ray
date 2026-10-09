// Package adaptive loads, trains, and applies the recording-trained activity
// classifier used by the Rust sensing server.
package adaptive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ruvnet/ruview/backend/internal/signal"
)

const (
	FeatureCount           = 15
	TrainedModelVersion    = 2
	RecordedAmplitudeLimit = 56
	maxModelFileBytes      = 32 << 20
)

var ErrInvalidModel = errors.New("adaptive model is invalid")

type ClassStats struct {
	Label  string                `json:"label"`
	Count  int                   `json:"count"`
	Mean   [FeatureCount]float64 `json:"mean"`
	Stddev [FeatureCount]float64 `json:"stddev"`
}

type Model struct {
	ClassStats         []ClassStats          `json:"class_stats"`
	Weights            [][]float64           `json:"weights"`
	GlobalMean         [FeatureCount]float64 `json:"global_mean"`
	GlobalStd          [FeatureCount]float64 `json:"global_std"`
	TrainedFrames      int                   `json:"trained_frames"`
	TrainingAccuracy   float64               `json:"training_accuracy"`
	ValidationAccuracy float64               `json:"validation_accuracy,omitempty"`
	ValidationSamples  int                   `json:"validation_samples,omitempty"`
	ValidationSplit    string                `json:"validation_split,omitempty"`
	Version            uint32                `json:"version"`
	ClassNames         []string              `json:"class_names"`
}

func (m *Model) Clone() *Model {
	if m == nil {
		return nil
	}
	copy := *m
	copy.ClassStats = append([]ClassStats(nil), m.ClassStats...)
	copy.ClassNames = append([]string(nil), m.ClassNames...)
	copy.Weights = make([][]float64, len(m.Weights))
	for i := range m.Weights {
		copy.Weights[i] = append([]float64(nil), m.Weights[i]...)
	}
	return &copy
}

func Load(path string) (*Model, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxModelFileBytes {
		return nil, fmt.Errorf("%w: unsupported file type or size", ErrInvalidModel)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxModelFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read adaptive model: %w", err)
	}
	if len(data) > maxModelFileBytes {
		return nil, fmt.Errorf("%w: file exceeds size limit", ErrInvalidModel)
	}
	var model Model
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&model); err != nil {
		return nil, fmt.Errorf("decode adaptive model: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON data", ErrInvalidModel)
	}
	if err := model.Validate(); err != nil {
		return nil, err
	}
	return &model, nil
}

func (m *Model) Validate() error {
	if m == nil || m.TrainedFrames < 1 || len(m.ClassNames) == 0 || len(m.ClassNames) > 64 || len(m.Weights) != len(m.ClassNames) || len(m.ClassStats) != len(m.ClassNames) {
		return fmt.Errorf("%w: inconsistent model dimensions", ErrInvalidModel)
	}
	if m.Version == 0 || m.Version > TrainedModelVersion || !finite(m.TrainingAccuracy) || m.TrainingAccuracy < 0 || m.TrainingAccuracy > 1 || !finite(m.ValidationAccuracy) || m.ValidationAccuracy < 0 || m.ValidationAccuracy > 1 || m.ValidationSamples < 0 {
		return fmt.Errorf("%w: invalid model metadata", ErrInvalidModel)
	}
	if m.ValidationSamples == 0 && (m.ValidationAccuracy != 0 || m.ValidationSplit != "") {
		return fmt.Errorf("%w: validation metrics require validation samples", ErrInvalidModel)
	}
	if m.ValidationSamples > 0 && m.ValidationSplit == "" {
		return fmt.Errorf("%w: validation split is required when validation samples are present", ErrInvalidModel)
	}
	seen := make(map[string]struct{}, len(m.ClassNames))
	for i, label := range m.ClassNames {
		if strings.TrimSpace(label) == "" || len(label) > 64 || !utf8.ValidString(label) {
			return fmt.Errorf("%w: invalid class name", ErrInvalidModel)
		}
		if _, exists := seen[label]; exists {
			return fmt.Errorf("%w: duplicate class name", ErrInvalidModel)
		}
		seen[label] = struct{}{}
		if len(m.Weights[i]) != FeatureCount+1 {
			return fmt.Errorf("%w: invalid weight vector for class %q", ErrInvalidModel, label)
		}
		for _, value := range m.Weights[i] {
			if !finite(value) || math.Abs(value) > 1e6 {
				return fmt.Errorf("%w: non-finite or out-of-range weight", ErrInvalidModel)
			}
		}
		stats := m.ClassStats[i]
		if stats.Label != label || stats.Count < 1 {
			return fmt.Errorf("%w: class statistics do not match class names", ErrInvalidModel)
		}
		for feature := 0; feature < FeatureCount; feature++ {
			if !finite(stats.Mean[feature]) || !finite(stats.Stddev[feature]) || stats.Stddev[feature] < 0 {
				return fmt.Errorf("%w: invalid class statistics", ErrInvalidModel)
			}
		}
	}
	for feature := 0; feature < FeatureCount; feature++ {
		if !finite(m.GlobalMean[feature]) || !finite(m.GlobalStd[feature]) || m.GlobalStd[feature] <= 0 {
			return fmt.Errorf("%w: invalid normalization statistics", ErrInvalidModel)
		}
	}
	return nil
}

func (m *Model) Save(path string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create adaptive model directory: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode adaptive model: %w", err)
	}
	if len(data) > maxModelFileBytes {
		return fmt.Errorf("%w: serialized model exceeds size limit", ErrInvalidModel)
	}
	file, err := os.CreateTemp(dir, ".adaptive-model-*.tmp")
	if err != nil {
		return fmt.Errorf("create adaptive model temp file: %w", err)
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure adaptive model temp file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write adaptive model: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync adaptive model: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close adaptive model: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace adaptive model: %w", err)
	}
	directory, err := os.Open(dir)
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	if err != nil {
		return fmt.Errorf("sync adaptive model directory: %w", err)
	}
	return nil
}

func (m *Model) Classify(features [FeatureCount]float64) (string, float64, error) {
	if err := m.Validate(); err != nil {
		return "", 0, err
	}
	normalized := [FeatureCount]float64{}
	for i, value := range features {
		if !finite(value) {
			return "", 0, fmt.Errorf("%w: non-finite input feature", ErrInvalidModel)
		}
		normalized[i] = (value - m.GlobalMean[i]) / (m.GlobalStd[i] + 1e-9)
	}
	logits := make([]float64, len(m.Weights))
	maxLogit := math.Inf(-1)
	for class, weights := range m.Weights {
		logit := weights[FeatureCount]
		for feature := 0; feature < FeatureCount; feature++ {
			logit += weights[feature] * normalized[feature]
		}
		if !finite(logit) {
			return "", 0, fmt.Errorf("%w: model produced a non-finite score", ErrInvalidModel)
		}
		logits[class] = logit
		if logit > maxLogit {
			maxLogit = logit
		}
	}
	expSum := 0.0
	bestClass, bestProbability := 0, -1.0
	for class, logit := range logits {
		probability := math.Exp(logit - maxLogit)
		expSum += probability
		logits[class] = probability
	}
	if !finite(expSum) || expSum <= 0 {
		return "", 0, fmt.Errorf("%w: invalid probability sum", ErrInvalidModel)
	}
	for class, probability := range logits {
		probability /= expSum
		if probability > bestProbability {
			bestClass, bestProbability = class, probability
		}
	}
	return m.ClassNames[bestClass], bestProbability, nil
}

// Apply overrides the heuristic motion class only when a valid model is loaded.
func (m *Model) Apply(observation *signal.Observation) error {
	if m == nil || observation == nil {
		return nil
	}
	dominantFrequency := observation.Features.DominantFreqHz
	if m.Version < 2 {
		dominantFrequency = legacyDominantFrequency(observation.Amplitude)
	}
	features := RuntimeFeatures(observation.Features, dominantFrequency, observation.Amplitude)
	label, confidence, err := m.Classify(features)
	if err != nil {
		return err
	}
	observation.Class.MotionLevel = label
	observation.Class.Presence = label != "absent"
	observation.Class.Confidence = clamp(confidence*0.7+observation.Class.Confidence*0.3, 0, 1)
	return nil
}

func RuntimeFeatures(features signal.FeatureInfo, dominantFrequency float64, amplitudes []float64) [FeatureCount]float64 {
	if len(amplitudes) > RecordedAmplitudeLimit {
		amplitudes = amplitudes[:RecordedAmplitudeLimit]
	}
	mean, std, skew, kurt, iqr, entropy, maximum, amplitudeRange := subcarrierStats(amplitudes)
	return [FeatureCount]float64{
		features.Variance, features.MotionBandPower, features.BreathingBandPower,
		features.SpectralPower, dominantFrequency, float64(features.ChangePoints), features.MeanRSSI,
		mean, std, skew, kurt, iqr, entropy, maximum, amplitudeRange,
	}
}

func legacyDominantFrequency(amplitudes []float64) float64 {
	bestIndex, best := -1, math.Inf(-1)
	if len(amplitudes) > RecordedAmplitudeLimit {
		amplitudes = amplitudes[:RecordedAmplitudeLimit]
	}
	for i, value := range amplitudes {
		if finite(value) && value > best {
			bestIndex, best = i, value
		}
	}
	if bestIndex < 0 {
		return 0
	}
	return float64(bestIndex) * 0.05
}

func subcarrierStats(values []float64) (float64, float64, float64, float64, float64, float64, float64, float64) {
	if len(values) == 0 {
		return 0, 0, 0, 0, 0, 0, 0, 0
	}
	copyValues := append([]float64(nil), values...)
	for _, value := range copyValues {
		if !finite(value) {
			return 0, 0, 0, 0, 0, 0, 0, 0
		}
	}
	n := float64(len(copyValues))
	mean := 0.0
	for _, value := range copyValues {
		mean += value
	}
	mean /= n
	variance := 0.0
	for _, value := range copyValues {
		variance += (value - mean) * (value - mean)
	}
	variance /= n
	std := math.Max(1e-9, math.Sqrt(variance))
	skew, kurt, power := 0.0, 0.0, 0.0
	for _, value := range copyValues {
		normalized := (value - mean) / std
		skew += normalized * normalized * normalized
		kurt += normalized * normalized * normalized * normalized
		power += value * value
	}
	skew /= n
	kurt = kurt/n - 3
	sortFloat64s(copyValues)
	iqr := copyValues[3*len(copyValues)/4] - copyValues[len(copyValues)/4]
	maxValue, rangeValue := copyValues[len(copyValues)-1], copyValues[len(copyValues)-1]-copyValues[0]
	totalPower := math.Max(1e-9, power)
	entropy := 0.0
	for _, value := range values {
		p := value * value / totalPower
		if p > 1e-12 {
			entropy -= p * math.Log(p)
		}
	}
	entropy /= math.Max(1e-9, math.Log(n))
	return mean, std, skew, kurt, iqr, entropy, maxValue, rangeValue
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func clamp(value, low, high float64) float64 {
	if !finite(value) {
		return low
	}
	return math.Max(low, math.Min(high, value))
}

func sortFloat64s(values []float64) {
	sort.Float64s(values)
}
