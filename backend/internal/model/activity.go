package model

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ruvnet/ruview/backend/internal/adaptive"
)

const (
	ActivityArchitecture  = "ruview.activity-softmax.v2"
	ActivityFeatureSchema = "ruview.activity.v2"
)

type activityModelMetadata struct {
	Architecture       string                         `json:"architecture"`
	FeatureSchema      string                         `json:"feature_schema"`
	WeightLayout       string                         `json:"weight_layout"`
	FeatureCount       int                            `json:"feature_count"`
	ClassNames         []string                       `json:"class_names"`
	ClassStats         []adaptive.ClassStats          `json:"class_stats"`
	GlobalMean         [adaptive.FeatureCount]float64 `json:"global_mean"`
	GlobalStd          [adaptive.FeatureCount]float64 `json:"global_std"`
	TrainedFrames      int                            `json:"trained_frames"`
	TrainingAccuracy   float64                        `json:"training_accuracy_in_sample"`
	ValidationAccuracy float64                        `json:"validation_accuracy"`
	ValidationSamples  int                            `json:"validation_samples"`
	ValidationSplit    string                         `json:"validation_split"`
}

// BuildActivityRVF packages a validated 15-feature softmax model in a typed
// RVF contract. The class order and row-major weights are part of that
// contract; arbitrary RVF vectors are never guessed to be executable models.
func BuildActivityRVF(id string, trained *adaptive.Model) ([]byte, error) {
	if !validID(id) {
		return nil, ErrInvalidID
	}
	if err := trained.Validate(); err != nil {
		return nil, fmt.Errorf("validate activity model: %w", err)
	}
	metadata := activityModelMetadata{
		Architecture: ActivityArchitecture, FeatureSchema: ActivityFeatureSchema,
		WeightLayout: "classes_x_16_row_major", FeatureCount: adaptive.FeatureCount,
		ClassNames: append([]string(nil), trained.ClassNames...),
		ClassStats: append([]adaptive.ClassStats(nil), trained.ClassStats...),
		GlobalMean: trained.GlobalMean, GlobalStd: trained.GlobalStd,
		TrainedFrames: trained.TrainedFrames, TrainingAccuracy: trained.TrainingAccuracy,
		ValidationAccuracy: trained.ValidationAccuracy, ValidationSamples: trained.ValidationSamples,
		ValidationSplit: trained.ValidationSplit,
	}
	manifest, err := json.Marshal(map[string]any{
		"model_id": id, "version": "1.0.0", "description": "Wave Ray supervised activity classifier",
		"created_at": time.Now().UTC().Format(time.RFC3339Nano), "inference": metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("encode activity model manifest: %w", err)
	}
	weights := make([]byte, 0, len(trained.Weights)*(adaptive.FeatureCount+1)*4)
	for _, row := range trained.Weights {
		for _, value := range row {
			weight := float32(value)
			if math.IsNaN(float64(weight)) || math.IsInf(float64(weight), 0) {
				return nil, errors.New("activity model weight cannot be represented as float32")
			}
			var encoded [4]byte
			binary.LittleEndian.PutUint32(encoded[:], math.Float32bits(weight))
			weights = append(weights, encoded[:]...)
		}
	}
	container := BuildRVF(
		Segment{Type: segmentManifest, Payload: manifest},
		Segment{Type: segmentVector, Payload: weights},
	)
	return container, nil
}

// compileActivity returns an executable model only for the explicitly typed
// architecture above. A recognized architecture with malformed metadata is
// rejected rather than silently downgraded to inert weights.
func compileActivity(container *Container) (*adaptive.Model, bool, error) {
	if container == nil {
		return nil, false, errors.New("model container is nil")
	}
	manifest := container.Manifest()
	inferenceValue, ok := manifest["inference"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	architecture, _ := inferenceValue["architecture"].(string)
	if architecture != ActivityArchitecture {
		return nil, false, nil
	}
	encoded, err := json.Marshal(inferenceValue)
	if err != nil {
		return nil, false, fmt.Errorf("encode activity model metadata: %w", err)
	}
	var metadata activityModelMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return nil, false, fmt.Errorf("decode activity model metadata: %w", err)
	}
	if metadata.FeatureSchema != ActivityFeatureSchema || metadata.FeatureCount != adaptive.FeatureCount || metadata.WeightLayout != "classes_x_16_row_major" {
		return nil, false, errors.New("unsupported activity model feature schema or weight layout")
	}
	if len(metadata.ClassNames) == 0 || len(metadata.ClassNames) > 64 || len(metadata.ClassStats) != len(metadata.ClassNames) || metadata.TrainedFrames < 1 {
		return nil, false, errors.New("activity model metadata has inconsistent dimensions")
	}
	weightData := container.Find(segmentVector)
	wantWeights := len(metadata.ClassNames) * (adaptive.FeatureCount + 1)
	if len(weightData) != wantWeights*4 {
		return nil, false, fmt.Errorf("activity model has %d weights; expected %d", len(weightData)/4, wantWeights)
	}
	modelWeights := make([][]float64, len(metadata.ClassNames))
	for class := range modelWeights {
		modelWeights[class] = make([]float64, adaptive.FeatureCount+1)
		for feature := range modelWeights[class] {
			index := class*(adaptive.FeatureCount+1) + feature
			value := math.Float32frombits(binary.LittleEndian.Uint32(weightData[index*4 : index*4+4]))
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, false, fmt.Errorf("activity model has a non-finite weight at index %d", index)
			}
			modelWeights[class][feature] = float64(value)
		}
	}
	compiled := &adaptive.Model{
		ClassNames: append([]string(nil), metadata.ClassNames...),
		ClassStats: append([]adaptive.ClassStats(nil), metadata.ClassStats...),
		GlobalMean: metadata.GlobalMean, GlobalStd: metadata.GlobalStd,
		Weights: modelWeights, TrainedFrames: metadata.TrainedFrames,
		TrainingAccuracy: metadata.TrainingAccuracy, ValidationAccuracy: metadata.ValidationAccuracy,
		ValidationSamples: metadata.ValidationSamples, ValidationSplit: metadata.ValidationSplit,
		Version: adaptive.TrainedModelVersion,
	}
	if err := compiled.Validate(); err != nil {
		return nil, false, fmt.Errorf("invalid compiled activity model: %w", err)
	}
	return compiled, true, nil
}
