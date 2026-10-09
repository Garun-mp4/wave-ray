package httpapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/model"
	posemodel "github.com/ruvnet/ruview/backend/internal/pose"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/recording"
	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/state"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func TestPoseAPIExposesExperimental2DButNeverClaimsPeopleOrMeters(t *testing.T) {
	hub := ws.NewHub()
	store := state.New("simulate", hub)
	pose, err := posemodel.LoadBytes(httpTestZeroPoseModel(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPoseModel(pose, nil); err != nil {
		t.Fatal(err)
	}
	dataDir, uiDir, modelsDir := t.TempDir(), t.TempDir(), t.TempDir()
	hosts, err := security.NewHostAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewRegistry(modelsDir)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := recording.New(dataDir, hub, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BindAddr: "127.0.0.1", HTTPPort: 8080, WSPort: 0, UDPBindAddr: "127.0.0.1",
		UDPPort: 5005, DisableUDP: true, Source: "simulate", UIPath: uiDir, DataDir: dataDir,
		ModelsDir: modelsDir, PoseNodeID: -1, NoEdgeRegistry: true,
	}
	api := New(cfg, store, security.NewTicketStore(), hosts, models, recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	for sequence := uint32(1); sequence <= posemodel.WindowFrames; sequence++ {
		amplitude, phase := make([]float64, posemodel.Subcarriers), make([]float64, posemodel.Subcarriers)
		for tone := range amplitude {
			amplitude[tone] = 1 + float64(tone)*0.01
		}
		store.IngestCSI(protocol.CSIFrame{
			Source: "simulate", NodeID: 1, Antennas: 1, Subcarriers: posemodel.Subcarriers,
			Sequence: sequence, RSSIDBm: -50, Amplitude: amplitude, Phase: phase,
		})
	}

	response, err := http.Get(server.URL + "/api/v1/pose/current")
	if err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || current["image_pose_available"] != true || current["person_detection_available"] != false || current["localization_available"] != false || current["metric_candidate_available"] != false || current["hotspots_are_people"] != false {
		t.Fatalf("pose endpoint overclaimed an experimental output: status=%d body=%v", response.StatusCode, current)
	}
	persons, ok := current["persons"].([]any)
	if !ok || len(persons) != 0 {
		t.Fatalf("single-person regressor was returned as person detections: %T %v", current["persons"], current["persons"])
	}
	if _, ok := current["experimental_pose"].(map[string]any); !ok {
		t.Fatalf("image-space model output is missing: %v", current)
	}

	response, err = http.Get(server.URL + "/api/v1/pose/calibration/status")
	if err != nil {
		t.Fatal(err)
	}
	var calibrationStatus map[string]any
	if err := json.NewDecoder(response.Body).Decode(&calibrationStatus); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || calibrationStatus["metric_transform_available"] != false || calibrationStatus["person_localization_available"] != false {
		t.Fatalf("uncalibrated model incorrectly reports metric localization: status=%d body=%v", response.StatusCode, calibrationStatus)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/pose/calibrate", bytes.NewBufferString(`{"action":"capture","room_x_m":1,"room_y_m":2}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]any
	if err := json.NewDecoder(response.Body).Decode(&capture); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict || capture["code"] != "hardware_evidence_required" {
		t.Fatalf("synthetic data was allowed to calibrate real metric coordinates: status=%d body=%v", response.StatusCode, capture)
	}

	response, err = http.Get(server.URL + "/api/v1/pose/current?view=refined")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("refined metric pose was returned without a validated localization model: %d", response.StatusCode)
	}
}

func TestPoseAPIDisabledModeDoesNotTurnSignalHotspotsIntoPeople(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	store.Refresh()
	response, err := http.Get(server.URL + "/api/v1/pose/current")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var current map[string]any
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || current["hotspots_are_people"] != false || current["person_detection_available"] != false || current["localization_available"] != false {
		t.Fatalf("disabled pose API emitted person/location claims: status=%d body=%v", response.StatusCode, current)
	}
}

func httpTestZeroPoseModel(t *testing.T) []byte {
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
