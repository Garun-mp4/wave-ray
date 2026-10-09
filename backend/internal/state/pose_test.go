package state

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	posemodel "github.com/ruvnet/ruview/backend/internal/pose"
	"github.com/ruvnet/ruview/backend/internal/protocol"
)

func TestPoseInferenceUsesOnlyContiguousSingleNodeRawCSI(t *testing.T) {
	model, err := posemodel.LoadBytes(zeroPoseModel(t))
	if err != nil {
		t.Fatal(err)
	}
	store := New("esp32", nil)
	if err := store.SetPoseModel(model, nil); err != nil {
		t.Fatal(err)
	}
	for sequence := uint32(1); sequence <= posemodel.WindowFrames; sequence++ {
		store.IngestCSI(rawPoseFrame(1, sequence, "simulate", ""))
	}
	snapshot := store.PoseInference(time.Now())
	if !snapshot.Enabled || snapshot.Status != "experimental" || snapshot.Estimate == nil || snapshot.Estimate.Evidence != "synthetic" {
		t.Fatalf("single-node raw CSI did not produce a correctly labeled image-space inference: %+v", snapshot)
	}
	if snapshot.Estimate.Prediction.Space != "normalized_image" || snapshot.Model.QualityValidated {
		t.Fatalf("inference was mislabeled as accepted metric pose: %+v", snapshot)
	}
}

func TestPoseInferenceRequiresNodeSelectionForMultipleRawSensors(t *testing.T) {
	model, err := posemodel.LoadBytes(zeroPoseModel(t))
	if err != nil {
		t.Fatal(err)
	}
	store := New("esp32", nil)
	if err := store.SetPoseModel(model, nil); err != nil {
		t.Fatal(err)
	}
	for sequence := uint32(1); sequence <= posemodel.WindowFrames; sequence++ {
		store.IngestCSI(rawPoseFrame(1, sequence, "esp32", ""))
		store.IngestCSI(rawPoseFrame(2, sequence, "esp32", ""))
	}
	snapshot := store.PoseInference(time.Now())
	if snapshot.Estimate != nil || snapshot.Reason != "pose_node_selection_required_for_multiple_sensors" {
		t.Fatalf("ambiguous multi-sensor pose inference was not blocked: %+v", snapshot)
	}
	selectedNode := uint8(2)
	if err := store.SetPoseModel(model, &selectedNode); err != nil {
		t.Fatal(err)
	}
	for sequence := uint32(1); sequence <= posemodel.WindowFrames; sequence++ {
		store.IngestCSI(rawPoseFrame(2, sequence, "esp32", ""))
	}
	snapshot = store.PoseInference(time.Now())
	if snapshot.Estimate == nil || snapshot.Estimate.NodeID != selectedNode {
		t.Fatalf("explicit node selection did not enable one-node inference: %+v", snapshot)
	}
}

func TestPoseInferenceRejectsRSSIDerivedPseudoCSI(t *testing.T) {
	model, err := posemodel.LoadBytes(zeroPoseModel(t))
	if err != nil {
		t.Fatal(err)
	}
	store := New("wifi", nil)
	if err := store.SetPoseModel(model, nil); err != nil {
		t.Fatal(err)
	}
	for sequence := uint32(1); sequence <= posemodel.WindowFrames; sequence++ {
		store.IngestCSI(rawPoseFrame(1, sequence, "esp32", "rssi_derived"))
	}
	snapshot := store.PoseInference(time.Now())
	if snapshot.Estimate != nil || snapshot.Reason != "pose_inference_requires_raw_esp32_csi_not_rssi" {
		t.Fatalf("RSSI-derived pseudo-CSI was used for pose inference: %+v", snapshot)
	}
}

func rawPoseFrame(nodeID uint8, sequence uint32, source, inputMode string) protocol.CSIFrame {
	amplitude := make([]float64, posemodel.Subcarriers)
	phase := make([]float64, posemodel.Subcarriers)
	for index := range amplitude {
		amplitude[index] = 1 + float64(index)*0.01
	}
	return protocol.CSIFrame{
		Source: source, InputMode: inputMode, NodeID: nodeID, Antennas: 1,
		Subcarriers: posemodel.Subcarriers, Sequence: sequence,
		RSSIDBm: -48, Amplitude: amplitude, Phase: phase,
	}
}

func zeroPoseModel(t *testing.T) []byte {
	t.Helper()
	type tensor struct {
		DType string `json:"dtype"`
		Shape []int  `json:"shape"`
		Range [2]int `json:"data_offsets"`
	}
	specs := []struct {
		name  string
		shape []int
	}{
		{"enc.c1.bias", []int{64}}, {"enc.c1.weight", []int{64, 56, 3}},
		{"enc.c2.bias", []int{128}}, {"enc.c2.weight", []int{128, 64, 3}},
		{"enc.c3.bias", []int{128}}, {"enc.c3.weight", []int{128, 128, 3}},
		{"head.fc1.bias", []int{256}}, {"head.fc1.weight", []int{256, 128}},
		{"head.fc2.bias", []int{34}}, {"head.fc2.weight", []int{34, 256}},
	}
	header := make(map[string]tensor, len(specs))
	var payload bytes.Buffer
	for _, spec := range specs {
		start := payload.Len()
		count := 1
		for _, dimension := range spec.shape {
			count *= dimension
		}
		for range count {
			if err := binary.Write(&payload, binary.LittleEndian, float32(0)); err != nil {
				t.Fatal(err)
			}
		}
		header[spec.name] = tensor{DType: "F32", Shape: spec.shape, Range: [2]int{start, payload.Len()}}
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	file := make([]byte, 8, 8+len(headerBytes)+payload.Len())
	binary.LittleEndian.PutUint64(file, uint64(len(headerBytes)))
	file = append(file, headerBytes...)
	return append(file, payload.Bytes()...)
}
