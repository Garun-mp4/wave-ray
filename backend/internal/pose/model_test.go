package pose

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
)

func TestLoadAndInferSafetensorsModel(t *testing.T) {
	model, err := LoadBytes(testSafetensors(t, false))
	if err != nil {
		t.Fatal(err)
	}
	prediction, err := model.Infer(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if prediction.Space != "normalized_image" || prediction.Keypoints[0] != (Point{X: 0.5, Y: 0.5}) {
		t.Fatalf("zero weights should produce the centered image-space skeleton: %+v", prediction)
	}
	info := model.Info()
	if info.MaxPersons != 1 || info.ConfidenceHead || info.CoordinateSpace != "normalized_image" || info.Validation.PCK20 != 0.03 {
		t.Fatalf("model capability and validation disclosure mismatch: %+v", info)
	}
}

func TestModelRejectsMalformedTensorFilesAndInvalidInput(t *testing.T) {
	fixture := testSafetensors(t, false)
	for name, data := range map[string][]byte{
		"truncated":        fixture[:len(fixture)-1],
		"oversized-header": append([]byte(nil), fixture[:8]...),
	} {
		if _, err := LoadBytes(data); err == nil {
			t.Errorf("%s fixture unexpectedly loaded", name)
		}
	}
	if _, err := LoadBytes(testSafetensors(t, true)); err == nil {
		t.Fatal("model with an unexpected tensor shape loaded")
	}

	model, err := LoadBytes(fixture)
	if err != nil {
		t.Fatal(err)
	}
	window := Window{}
	window[0][0] = float32(math.NaN())
	if _, err := model.Infer(window); err == nil {
		t.Fatal("non-finite CSI input was accepted")
	}
}

func testSafetensors(t *testing.T, badShape bool) []byte {
	t.Helper()
	type headerTensor struct {
		DType string `json:"dtype"`
		Shape []int  `json:"shape"`
		Range [2]int `json:"data_offsets"`
	}
	header := make(map[string]headerTensor, len(modelTensorSpecs))
	var payload bytes.Buffer
	for _, spec := range modelTensorSpecs {
		shape := append([]int(nil), spec.shape...)
		if badShape && spec.name == "head.fc2.bias" {
			shape[0]++
		}
		count := 1
		for _, dim := range spec.shape {
			count *= dim
		}
		start := payload.Len()
		for i := 0; i < count; i++ {
			if err := binary.Write(&payload, binary.LittleEndian, float32(0)); err != nil {
				t.Fatal(err)
			}
		}
		header[spec.name] = headerTensor{DType: "F32", Shape: shape, Range: [2]int{start, payload.Len()}}
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	file := make([]byte, 8, 8+len(headerBytes)+payload.Len())
	binary.LittleEndian.PutUint64(file, uint64(len(headerBytes)))
	file = append(file, headerBytes...)
	file = append(file, payload.Bytes()...)
	return file
}
