// Package pose provides bounded, CPU-only inference for the repository's
// experimental 17-keypoint Wi-Fi CSI pose model.
package pose

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
)

const (
	Subcarriers    = 56
	WindowFrames   = 20
	KeypointCount  = 17
	maxModelBytes  = 2 << 20
	maxHeaderBytes = 64 << 10
)

var ErrInvalidModel = errors.New("invalid pose model")

type Point struct {
	X float32 `json:"x"`
	Y float32 `json:"y"`
}

type Window [Subcarriers][WindowFrames]float32

type Prediction struct {
	Keypoints [KeypointCount]Point `json:"keypoints"`
	Space     string               `json:"coordinate_space"`
}

type Validation struct {
	PCK20            float64 `json:"pck20"`
	PCK50            float64 `json:"pck50"`
	Split            string  `json:"split"`
	Evidence         string  `json:"evidence"`
	Confidence       string  `json:"per_frame_confidence"`
	MeanPoseBaseline string  `json:"mean_pose_baseline"`
	AcceptanceStatus string  `json:"acceptance_status"`
}

type ModelInfo struct {
	ID               string     `json:"model_id"`
	ArtifactSHA256   string     `json:"artifact_sha256"`
	InputShape       [2]int     `json:"input_shape"`
	OutputKeypoints  int        `json:"output_keypoints"`
	CoordinateSpace  string     `json:"coordinate_space"`
	MaxPersons       int        `json:"max_persons"`
	ConfidenceHead   bool       `json:"confidence_head"`
	QualityValidated bool       `json:"quality_validated"`
	Validation       Validation `json:"validation"`
}

type tensorDescriptor struct {
	DType       string   `json:"dtype"`
	Shape       []int    `json:"shape"`
	DataOffsets []uint64 `json:"data_offsets"`
}

type tensorSpec struct {
	name  string
	shape []int
}

var modelTensorSpecs = []tensorSpec{
	{name: "enc.c1.bias", shape: []int{64}},
	{name: "enc.c1.weight", shape: []int{64, 56, 3}},
	{name: "enc.c2.bias", shape: []int{128}},
	{name: "enc.c2.weight", shape: []int{128, 64, 3}},
	{name: "enc.c3.bias", shape: []int{128}},
	{name: "enc.c3.weight", shape: []int{128, 128, 3}},
	{name: "head.fc1.bias", shape: []int{256}},
	{name: "head.fc1.weight", shape: []int{256, 128}},
	{name: "head.fc2.bias", shape: []int{34}},
	{name: "head.fc2.weight", shape: []int{34, 256}},
}

type convLayer struct {
	in, out, kernel, dilation, padding int
	weights, bias                      []float32
}

type denseLayer struct {
	in, out       int
	weights, bias []float32
}

type Model struct {
	info ModelInfo
	c1   convLayer
	c2   convLayer
	c3   convLayer
	fc1  denseLayer
	fc2  denseLayer
}

// Load reads the repository's safetensors model after rejecting symlinks,
// oversized files, malformed tensor layouts, and non-finite weights.
func Load(path string) (*Model, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat pose model: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 8 || info.Size() > maxModelBytes {
		return nil, fmt.Errorf("%w: model must be a regular file no larger than %d bytes", ErrInvalidModel, maxModelBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pose model: %w", err)
	}
	return LoadBytes(data)
}

func LoadBytes(data []byte) (*Model, error) {
	tensors, err := decodeSafetensors(data)
	if err != nil {
		return nil, err
	}
	get := func(name string) []float32 { return tensors[name] }
	model := &Model{
		c1:  convLayer{in: 56, out: 64, kernel: 3, dilation: 1, padding: 1, weights: get("enc.c1.weight"), bias: get("enc.c1.bias")},
		c2:  convLayer{in: 64, out: 128, kernel: 3, dilation: 2, padding: 2, weights: get("enc.c2.weight"), bias: get("enc.c2.bias")},
		c3:  convLayer{in: 128, out: 128, kernel: 3, dilation: 4, padding: 4, weights: get("enc.c3.weight"), bias: get("enc.c3.bias")},
		fc1: denseLayer{in: 128, out: 256, weights: get("head.fc1.weight"), bias: get("head.fc1.bias")},
		fc2: denseLayer{in: 256, out: 34, weights: get("head.fc2.weight"), bias: get("head.fc2.bias")},
	}
	hash := sha256.Sum256(data)
	model.info = ModelInfo{
		ID: "pose_v1", ArtifactSHA256: hex.EncodeToString(hash[:]),
		InputShape: [2]int{Subcarriers, WindowFrames}, OutputKeypoints: KeypointCount,
		CoordinateSpace: "normalized_image", MaxPersons: 1, ConfidenceHead: false, QualityValidated: false,
		Validation: Validation{
			PCK20: 0.03, PCK50: 0.185, Split: "temporal_80_20",
			Evidence:   "repository_measured_not_acceptance_ready_missing_mean_pose_baseline",
			Confidence: "unavailable_no_confidence_head", MeanPoseBaseline: "not_reported",
			AcceptanceStatus: "not_accepted_for_person_localization",
		},
	}
	return model, nil
}

// FloorPoint returns the midpoint of the model's two nominal ankle channels.
// It is an image-plane regression output, not a detected person or an
// independently validated foot contact point.
func (p Prediction) FloorPoint() Point {
	return Point{
		X: (p.Keypoints[15].X + p.Keypoints[16].X) / 2,
		Y: (p.Keypoints[15].Y + p.Keypoints[16].Y) / 2,
	}
}

func (m *Model) Info() ModelInfo {
	if m == nil {
		return ModelInfo{}
	}
	return m.info
}

// Infer returns normalized 2D keypoints. This model has no person detector,
// per-joint confidence head, depth output, or multi-person output.
func (m *Model) Infer(window Window) (Prediction, error) {
	if m == nil {
		return Prediction{}, errors.New("pose model is not loaded")
	}
	input := make([][]float32, Subcarriers)
	for channel := range input {
		input[channel] = make([]float32, WindowFrames)
		for frame, value := range window[channel] {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) || math.Abs(float64(value)) > 1e6 {
				return Prediction{}, errors.New("pose input contains an invalid CSI value")
			}
			input[channel][frame] = value
		}
	}

	h := reluConv(m.c1, input)
	h = reluConv(m.c2, h)
	h = reluConv(m.c3, h)
	pooled := make([]float32, len(h))
	for channel, values := range h {
		for _, value := range values {
			pooled[channel] += value / WindowFrames
		}
	}
	h1 := reluDense(m.fc1, pooled)
	out := dense(m.fc2, h1)
	var prediction Prediction
	prediction.Space = "normalized_image"
	for i := range prediction.Keypoints {
		x, y := sigmoid(out[i*2]), sigmoid(out[i*2+1])
		if !finite32(x) || !finite32(y) {
			return Prediction{}, errors.New("pose model produced a non-finite keypoint")
		}
		prediction.Keypoints[i] = Point{X: x, Y: y}
	}
	return prediction, nil
}

func decodeSafetensors(data []byte) (map[string][]float32, error) {
	if len(data) < 8 || len(data) > maxModelBytes {
		return nil, fmt.Errorf("%w: invalid file size", ErrInvalidModel)
	}
	headerLength := binary.LittleEndian.Uint64(data[:8])
	if headerLength == 0 || headerLength > maxHeaderBytes || headerLength > uint64(len(data)-8) {
		return nil, fmt.Errorf("%w: invalid header size", ErrInvalidModel)
	}
	headerEnd := 8 + int(headerLength)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data[8:headerEnd], &raw); err != nil {
		return nil, fmt.Errorf("%w: decode header: %v", ErrInvalidModel, err)
	}
	if len(raw) != len(modelTensorSpecs) && len(raw) != len(modelTensorSpecs)+1 {
		return nil, fmt.Errorf("%w: expected %d tensors", ErrInvalidModel, len(modelTensorSpecs))
	}
	if metadata, ok := raw["__metadata__"]; ok {
		var values map[string]string
		if err := json.Unmarshal(metadata, &values); err != nil {
			return nil, fmt.Errorf("%w: invalid metadata", ErrInvalidModel)
		}
		delete(raw, "__metadata__")
	}
	if len(raw) != len(modelTensorSpecs) {
		return nil, fmt.Errorf("%w: unexpected tensor set", ErrInvalidModel)
	}

	type tensorRange struct {
		name       string
		start, end uint64
	}
	ranges := make([]tensorRange, 0, len(modelTensorSpecs))
	descriptors := make(map[string]tensorDescriptor, len(modelTensorSpecs))
	for _, spec := range modelTensorSpecs {
		encoded, ok := raw[spec.name]
		if !ok {
			return nil, fmt.Errorf("%w: missing tensor %s", ErrInvalidModel, spec.name)
		}
		var descriptor tensorDescriptor
		if err := json.Unmarshal(encoded, &descriptor); err != nil {
			return nil, fmt.Errorf("%w: invalid tensor %s", ErrInvalidModel, spec.name)
		}
		if descriptor.DType != "F32" || !equalInts(descriptor.Shape, spec.shape) || len(descriptor.DataOffsets) != 2 {
			return nil, fmt.Errorf("%w: tensor %s has an unsupported type or shape", ErrInvalidModel, spec.name)
		}
		count := 1
		for _, dimension := range spec.shape {
			count *= dimension
		}
		start, end := descriptor.DataOffsets[0], descriptor.DataOffsets[1]
		if end < start || end-start != uint64(count*4) {
			return nil, fmt.Errorf("%w: tensor %s has an invalid byte range", ErrInvalidModel, spec.name)
		}
		ranges = append(ranges, tensorRange{name: spec.name, start: start, end: end})
		descriptors[spec.name] = descriptor
	}
	dataBytes := uint64(len(data) - headerEnd)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	var expectedOffset uint64
	for _, entry := range ranges {
		if entry.start != expectedOffset || entry.end > dataBytes {
			return nil, fmt.Errorf("%w: tensor byte ranges overlap or leave gaps", ErrInvalidModel)
		}
		expectedOffset = entry.end
	}
	if expectedOffset != dataBytes {
		return nil, fmt.Errorf("%w: unreferenced tensor data", ErrInvalidModel)
	}

	tensors := make(map[string][]float32, len(modelTensorSpecs))
	for _, spec := range modelTensorSpecs {
		descriptor := descriptors[spec.name]
		start := uint64(headerEnd) + descriptor.DataOffsets[0]
		end := uint64(headerEnd) + descriptor.DataOffsets[1]
		values := make([]float32, int((end-start)/4))
		for i := range values {
			value := math.Float32frombits(binary.LittleEndian.Uint32(data[int(start)+i*4 : int(start)+i*4+4]))
			if !finite32(value) {
				return nil, fmt.Errorf("%w: tensor %s contains a non-finite value", ErrInvalidModel, spec.name)
			}
			values[i] = value
		}
		tensors[spec.name] = values
	}
	return tensors, nil
}

func reluConv(layer convLayer, input [][]float32) [][]float32 {
	output := make([][]float32, layer.out)
	for outChannel := 0; outChannel < layer.out; outChannel++ {
		values := make([]float32, WindowFrames)
		for timestep := 0; timestep < WindowFrames; timestep++ {
			sum := layer.bias[outChannel]
			for inChannel := 0; inChannel < layer.in; inChannel++ {
				for kernelIndex := 0; kernelIndex < layer.kernel; kernelIndex++ {
					inputIndex := timestep - layer.padding + kernelIndex*layer.dilation
					if inputIndex < 0 || inputIndex >= WindowFrames {
						continue
					}
					weightIndex := (outChannel*layer.in+inChannel)*layer.kernel + kernelIndex
					sum += layer.weights[weightIndex] * input[inChannel][inputIndex]
				}
			}
			if sum > 0 {
				values[timestep] = sum
			}
		}
		output[outChannel] = values
	}
	return output
}

func reluDense(layer denseLayer, input []float32) []float32 {
	output := dense(layer, input)
	for i := range output {
		if output[i] < 0 {
			output[i] = 0
		}
	}
	return output
}

func dense(layer denseLayer, input []float32) []float32 {
	output := make([]float32, layer.out)
	for out := 0; out < layer.out; out++ {
		sum := layer.bias[out]
		for in, value := range input {
			sum += layer.weights[out*layer.in+in] * value
		}
		output[out] = sum
	}
	return output
}

func sigmoid(value float32) float32 {
	if value >= 0 {
		return float32(1 / (1 + math.Exp(-float64(value))))
	}
	exp := math.Exp(float64(value))
	return float32(exp / (1 + exp))
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func finite32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
