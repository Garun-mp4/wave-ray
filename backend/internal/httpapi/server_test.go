package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/calibration"
	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/recording"
	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/signal"
	"github.com/ruvnet/ruview/backend/internal/state"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func testServer(t *testing.T, token string, privacy bool) (*httptest.Server, *state.Store, *ws.Hub) {
	server, store, hub, _ := testServerWithData(t, token, privacy)
	return server, store, hub
}

func testServerWithData(t *testing.T, token string, privacy bool) (*httptest.Server, *state.Store, *ws.Hub, string) {
	t.Helper()
	hub := ws.NewHub()
	store := state.New("auto", hub)
	hosts, err := security.NewHostAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	cfg := config.Config{
		BindAddr: "127.0.0.1", HTTPPort: 8080, WSPort: 8765,
		UDPBindAddr: "127.0.0.1", UDPPort: 5005, Source: "auto",
		UIPath: t.TempDir(), DataDir: dataDir, APIToken: token, PrivacyMode: privacy,
	}
	models, err := model.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := recording.New(cfg.DataDir, hub, privacy)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(cfg, store, security.NewTicketStore(), hosts, models, recorder, logger)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(api.Close)
	return server, store, hub, dataDir
}

func calibrationTestServer(t *testing.T) (*httptest.Server, *state.Store, *calibration.Manager) {
	t.Helper()
	store := state.New("auto", nil)
	hosts, err := security.NewHostAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: t.TempDir(), UIPath: t.TempDir(), Source: "auto"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(cfg, store, nil, hosts, nil, nil, logger)
	if api.Calibration == nil {
		t.Fatalf("calibration manager was not initialized: %v", api.calibrationErr)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(api.Close)
	return server, store, api.Calibration
}

func TestReadinessAndEmptyLatestEndpoints(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	for path, expected := range map[string]string{
		"/health/ready":          `"status":"ready"`,
		"/health/health":         `"status":"degraded"`,
		"/api/v1/sensing/latest": `"status":"no data yet"`,
		"/api/v1/nodes":          `"total":0`,
	} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), expected) {
			t.Fatalf("%s: status=%d body=%s", path, resp.StatusCode, body)
		}
	}
}

func TestHealthSystemLabelsSyntheticSource(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	store.SetSource("simulate")
	store.IngestCSI(protocol.CSIFrame{
		NodeID: 1, Antennas: 1, Subcarriers: 1, RSSIDBm: -50, NoiseFloor: -90,
		Amplitude: []float64{12}, Phase: []float64{0.1},
	})
	response, err := http.Get(server.URL + "/health/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health struct {
		Components map[string]struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"components"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	hardware := health.Components["hardware"]
	if response.StatusCode != http.StatusOK || hardware.Status != "simulated" || !strings.Contains(hardware.Message, "no live sensor data") {
		t.Fatalf("synthetic status was not explicit: status=%d hardware=%+v", response.StatusCode, hardware)
	}
}

func TestMigrationStatusEndpointsAreExplicit(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	response, err := http.Get(server.URL + "/oauth/status")
	if err != nil {
		t.Fatal(err)
	}
	var oauth map[string]any
	if err := json.NewDecoder(response.Body).Decode(&oauth); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || oauth["browser_signin"] != false || oauth["oauth_enabled"] != false {
		t.Fatalf("unexpected OAuth capability status=%d body=%v", response.StatusCode, oauth)
	}

	response, err = http.Post(server.URL+"/api/v1/train/start", "application/json", strings.NewReader(`{"dataset_ids":[],"config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	var training map[string]any
	if err := json.NewDecoder(response.Body).Decode(&training); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(training["detail"].(string), "dataset_ids") {
		t.Fatalf("empty training selection was not rejected: status=%d body=%v", response.StatusCode, training)
	}
}

func TestTrainingRESTLifecycleProducesLoadableRVFModel(t *testing.T) {
	server, _, _, dataDir := testServerWithData(t, "", false)
	recordingsDir := filepath.Join(dataDir, "recordings")
	write := func(name string, variance float64, amplitudes []float64) {
		t.Helper()
		file, err := os.Create(filepath.Join(recordingsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for frame := 0; frame < 8; frame++ {
			value := map[string]any{
				"features": map[string]any{
					"variance": variance, "motion_band_power": variance * 2,
					"breathing_band_power": 0.1, "spectral_power": variance + 1,
					"dominant_freq_hz": 0.25, "change_points": 2, "mean_rssi": -55,
				},
				"nodes": []any{map[string]any{"node_id": 1, "amplitude": amplitudes}},
			}
			if err := encoder.Encode(value); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("train_absent_room_a.jsonl", 0.1, []float64{1, 1, 1, 1})
	write("train_absent_room_b.jsonl", 0.2, []float64{1, 2, 1, 2})
	write("train_walking_room_a.jsonl", 10, []float64{2, 6, 4, 9})
	write("train_walking_room_b.jsonl", 11, []float64{3, 7, 5, 10})

	response, err := http.Get(server.URL + "/api/v1/datasets")
	if err != nil {
		t.Fatal(err)
	}
	var datasetListing struct {
		Datasets []struct {
			ID       string `json:"id"`
			Eligible bool   `json:"eligible"`
		} `json:"datasets"`
	}
	if err := json.NewDecoder(response.Body).Decode(&datasetListing); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(datasetListing.Datasets) != 4 {
		t.Fatalf("dataset listing status=%d datasets=%+v", response.StatusCode, datasetListing.Datasets)
	}
	ids := make([]string, 0, 4)
	for _, dataset := range datasetListing.Datasets {
		if !dataset.Eligible {
			t.Fatalf("valid labelled dataset was marked ineligible: %+v", dataset)
		}
		ids = append(ids, dataset.ID)
	}
	detailResponse, err := http.Get(server.URL + "/api/v1/datasets/" + ids[0])
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		detailResponse.Body.Close()
		t.Fatal(err)
	}
	detailResponse.Body.Close()
	if detailResponse.StatusCode != http.StatusOK || detail.ID != ids[0] || detail.Label == "" {
		t.Fatalf("dataset detail status=%d detail=%+v", detailResponse.StatusCode, detail)
	}
	payload, _ := json.Marshal(map[string]any{"dataset_ids": ids, "config": map[string]any{"epochs": 4}})
	response, err = http.Post(server.URL+"/api/v1/train/start", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("training did not start: status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	var status struct {
		Active             bool     `json:"active"`
		Phase              string   `json:"phase"`
		ModelID            string   `json:"model_id"`
		ValidationAccuracy *float64 `json:"validation_accuracy"`
		ValidationSplit    string   `json:"validation_split"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err = http.Get(server.URL + "/api/v1/train/status")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if !status.Active {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if status.Active || status.Phase != "completed" || status.ModelID == "" || status.ValidationAccuracy == nil || status.ValidationSplit != "recording_file_holdout" {
		t.Fatalf("training job did not complete with a held-out evaluation: %+v", status)
	}
	response, err = http.Get(server.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Models []model.ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&models); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	var found bool
	for _, item := range models.Models {
		if item.ID == status.ModelID && item.Loadable && item.InferenceAvailable {
			found = true
		}
	}
	if !found {
		t.Fatalf("trained model %q was not listed as executable: %+v", status.ModelID, models.Models)
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/models/load", strings.NewReader(`{"model_id":"`+status.ModelID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("trained model load status=%d", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/models/active")
	if err != nil {
		t.Fatal(err)
	}
	var active model.ActiveModel
	if err := json.NewDecoder(response.Body).Decode(&active); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !active.InferenceAvailable || active.Architecture != model.ActivityArchitecture {
		t.Fatalf("trained model was not activated for inference: %+v", active)
	}
	response, err = http.Post(server.URL+"/api/v1/train/stop", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stopping with no active job should return conflict; got %d", response.StatusCode)
	}
}

func TestUnimplementedPoseTrainingEndpointsFailExplicitly(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	for _, path := range []string{"/api/v1/train/pretrain", "/api/v1/train/lora"} {
		response, err := http.Post(server.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), `"status":"unavailable"`) || !strings.Contains(string(body), "pose-model training") {
			t.Fatalf("%s did not report the unsupported capability honestly: status=%d body=%s", path, response.StatusCode, body)
		}
	}
}

func TestTrainingProgressSocketPublishesCurrentJobState(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/train/progress"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test complete")
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(message, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["type"] != "training_progress" || payload["enabled"] != true || payload["phase"] != "idle" || payload["pose_training_enabled"] != false {
		t.Fatalf("training progress socket reported incorrect capabilities: %s", message)
	}
}

func TestUnmigratedCapabilitiesReturnHonestStatusInsteadOf404(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	store.IngestFeatureState(protocol.FeatureStatePacket{
		NodeID: 1, PresenceScore: 0.8, MotionScore: 0.2, HeartbeatBPM: 72,
		HeartbeatConf: 0.9, QualityFlags: 4,
	})

	for _, path := range []string{
		"/api/v1/pose/frames", "/api/v1/pose/zones", "/api/v1/calibration/status",
		"/api/v1/mesh", "/api/v1/rf/vendors/plume/latest", "/api/v1/edge-vitals",
		"/api/v1/vital-signs",
	} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK || strings.Contains(string(body), "404 page not found") {
			t.Fatalf("%s was not handled explicitly: status=%d body=%s", path, response.StatusCode, body)
		}
		if path == "/api/v1/edge-vitals" && (!strings.Contains(string(body), `"status":"abstained"`) || strings.Contains(string(body), "heart_rate_bpm")) {
			t.Fatalf("edge-vitals must fail closed until calibration state is migrated: %s", body)
		}
		if path == "/api/v1/vital-signs" && (!strings.Contains(string(body), `"authority":"abstained"`) || strings.Contains(string(body), "heartbeat_bpm")) {
			t.Fatalf("vital signs must fail closed until calibration state is migrated: %s", body)
		}
	}
}

func TestRoomCalibrationControlsAndTrustStatus(t *testing.T) {
	server, store, manager := calibrationTestServer(t)
	post := func(path, body string) (*http.Response, map[string]any) {
		t.Helper()
		response, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		return response, decoded
	}
	response, invalid := post("/api/v1/calibration/start", `{"binding_digest":"BAD","source_node_ids":[5]}`)
	if response.StatusCode != http.StatusBadRequest || invalid["error_code"] != "calibration_binding_invalid" {
		t.Fatalf("invalid calibration binding response=%d body=%v", response.StatusCode, invalid)
	}
	response, unavailable := post("/api/v1/calibration/start", `{"binding_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_node_ids":[5]}`)
	if response.StatusCode != http.StatusConflict || unavailable["error_code"] != "calibration_source_required" {
		t.Fatalf("start without grid evidence response=%d body=%v", response.StatusCode, unavailable)
	}
	gridNow := time.Now()
	for i := 0; i <= 30; i++ {
		manager.Observe(protocol.CSIFrame{
			NodeID: 5, Sequence: uint32(i), Subcarriers: 56,
			Amplitude: make([]float64, 56),
		}, gridNow.Add(-12*time.Second+time.Duration(i)*400*time.Millisecond))
	}
	response, started := post("/api/v1/calibration/start?source_node_id=5", `{"binding_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_node_ids":[5]}`)
	if response.StatusCode != http.StatusOK || started["success"] != true || started["status"] != "uncalibrated" || started["source_node_id"] != float64(5) {
		t.Fatalf("calibration did not start from eligible source: response=%d body=%v", response.StatusCode, started)
	}
	sourceIDs, ok := started["source_node_ids"].([]any)
	if !ok || len(sourceIDs) != 1 || sourceIDs[0] != float64(5) {
		t.Fatalf("start response must expose source IDs as a JSON number array: %T %v", started["source_node_ids"], started["source_node_ids"])
	}
	statusResponse, err := http.Get(server.URL + "/api/v1/calibration/status")
	if err != nil {
		t.Fatal(err)
	}
	var activeStatus map[string]any
	if err := json.NewDecoder(statusResponse.Body).Decode(&activeStatus); err != nil {
		statusResponse.Body.Close()
		t.Fatal(err)
	}
	statusResponse.Body.Close()
	missingIDs, ok := activeStatus["missing_source_node_ids"].([]any)
	if statusResponse.StatusCode != http.StatusOK || !ok || len(missingIDs) != 1 || missingIDs[0] != float64(5) {
		t.Fatalf("status must expose missing source IDs as a JSON number array: status=%d body=%v", statusResponse.StatusCode, activeStatus)
	}
	store.IngestCSI(protocol.CSIFrame{NodeID: 5, Sequence: 100, Subcarriers: 56, Amplitude: make([]float64, 56)})
	identity, err := json.Marshal(map[string]any{
		"boot_epoch": started["boot_epoch"], "session_id": started["session_id"],
		"binding_digest": started["binding_digest"], "source_node_ids": []int{5},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, cancelled := post("/api/v1/calibration/cancel", string(identity))
	if response.StatusCode != http.StatusOK || cancelled["success"] != true || cancelled["status"] != "none" {
		t.Fatalf("calibration cancel response=%d body=%v", response.StatusCode, cancelled)
	}
	response, staleIdentity := post("/api/v1/calibration/stop", string(identity))
	if response.StatusCode != http.StatusConflict || staleIdentity["error_code"] != "calibration_identity_mismatch" {
		t.Fatalf("stale identity response=%d body=%v", response.StatusCode, staleIdentity)
	}
	store.IngestVitals(protocol.VitalsPacket{NodeID: 5, Persons: 3, CountValid: true})
	store.IngestVitals(protocol.VitalsPacket{NodeID: 6, Persons: 3, CountValid: true})
	response, tuned := post("/api/v1/config/ground-truth", `{"count":1}`)
	if response.StatusCode != http.StatusOK || tuned["computed_dedup_factor"] != float64(6) || tuned["fresh_count_contributors"] != float64(2) || tuned["applied_to_count_fusion"] != true {
		t.Fatalf("ground-truth tuning response=%d body=%v", response.StatusCode, tuned)
	}
	get, err := http.Get(server.URL + "/api/v1/config/dedup-factor")
	if err != nil {
		t.Fatal(err)
	}
	var factor map[string]any
	if err := json.NewDecoder(get.Body).Decode(&factor); err != nil {
		get.Body.Close()
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusOK || factor["dedup_factor"] != float64(6) {
		t.Fatalf("dedup factor was not persisted: response=%d body=%v", get.StatusCode, factor)
	}
	trustResponse, err := http.Get(server.URL + "/api/v1/trust/status")
	if err != nil {
		t.Fatal(err)
	}
	var trust map[string]any
	if err := json.NewDecoder(trustResponse.Body).Decode(&trust); err != nil {
		trustResponse.Body.Close()
		t.Fatal(err)
	}
	trustResponse.Body.Close()
	if trustResponse.StatusCode != http.StatusOK || trust["authorized"] != false || trust["authority"] != "abstained" {
		t.Fatalf("trust endpoint authorized unvalidated measurements: response=%d body=%v", trustResponse.StatusCode, trust)
	}
	statusResponse, err = http.Get(server.URL + "/api/v1/calibration/status")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		statusResponse.Body.Close()
		t.Fatal(err)
	}
	statusResponse.Body.Close()
	bootEpoch, ok := status["boot_epoch"].(string)
	if !ok || bootEpoch == "" {
		t.Fatalf("status omitted boot epoch required for reset: %v", status)
	}
	response, reset := post("/api/v1/calibration/reset", `{"boot_epoch":"`+bootEpoch+`"}`)
	if response.StatusCode != http.StatusOK || reset["success"] != true || reset["status"] != "none" {
		t.Fatalf("boot-scoped calibration reset response=%d body=%v", response.StatusCode, reset)
	}
}

func TestWASMEventsRouteReturnsEmptyAndLatestDataStates(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	response, err := http.Get(server.URL + "/api/v1/wasm-events")
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]any
	if err := json.NewDecoder(response.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || empty["status"] != "no_data" || empty["wasm_events"] != nil {
		t.Fatalf("unexpected empty WASM response: status=%d body=%v", response.StatusCode, empty)
	}

	store.IngestWASMOutput(protocol.WASMOutputPacket{
		NodeID: 4, ModuleID: 2, Events: []protocol.WASMEvent{{EventType: 1, Value: .75}},
	})
	response, err = http.Get(server.URL + "/api/v1/wasm-events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var latest map[string]any
	if err := json.NewDecoder(response.Body).Decode(&latest); err != nil {
		t.Fatal(err)
	}
	events, ok := latest["wasm_events"].(map[string]any)
	if response.StatusCode != http.StatusOK || latest["status"] != "ok" || !ok || events["node_id"] != float64(4) || events["module_id"] != float64(2) {
		t.Fatalf("unexpected latest WASM response: status=%d body=%v", response.StatusCode, latest)
	}
}

func TestVendorRFDescriptorIngestAndLatestRoutes(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	response, err := http.Get(server.URL + "/api/v1/rf/vendors")
	if err != nil {
		t.Fatal(err)
	}
	var descriptors []map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&descriptors)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || len(descriptors) != 10 {
		t.Fatalf("vendor descriptors: status=%d count=%d error=%v", response.StatusCode, len(descriptors), decodeErr)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/rf/vendors/plume/events", strings.NewReader(`{"observations":[{"sequence":9,"timestamp_us":10,"source_id":"pod-a","rssi_dbm":-54,"channel":36,"clients":4}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]any
	decodeErr = json.NewDecoder(response.Body).Decode(&accepted)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusAccepted || accepted["accepted"] != float64(1) {
		t.Fatalf("vendor ingest: status=%d body=%v error=%v", response.StatusCode, accepted, decodeErr)
	}

	response, err = http.Get(server.URL + "/api/v1/rf/vendors/plume/latest")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	decodeErr = json.NewDecoder(response.Body).Decode(&snapshot)
	response.Body.Close()
	metrics, _ := snapshot["metrics"].(map[string]any)
	if decodeErr != nil || response.StatusCode != http.StatusOK || snapshot["event_type"] != "vendor_rf" || snapshot["source"] != "vendor:plume:live" || metrics["rssi_dbm"] != float64(-54) {
		t.Fatalf("vendor latest: status=%d body=%v error=%v", response.StatusCode, snapshot, decodeErr)
	}
	if response, err = http.Get(server.URL + "/api/v1/rf/vendors/unknown/latest"); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown vendor status=%d", response.StatusCode)
		}
	}
}

func TestVendorRFIngestEnforcesProviderAndPayloadLimits(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	for _, tc := range []struct {
		vendor  string
		payload string
		status  int
	}{
		{"linksys", `{}`, http.StatusNotImplemented},
		{"wifigarden", `{}`, http.StatusForbidden},
		{"plume", `{"observations":[{"sequence":1,"timestamp_us":2,"source_id":"x","rssi_dbm":4,"channel":1}]}`, http.StatusBadRequest},
		{"unknown", `{}`, http.StatusNotFound},
	} {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/rf/vendors/"+tc.vendor+"/events", strings.NewReader(tc.payload))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s status=%d, want %d", tc.vendor, response.StatusCode, tc.status)
		}
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/rf/vendors/plume/events", strings.NewReader(strings.Repeat("x", 1<<20+1)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized payload status=%d", response.StatusCode)
	}
}

func TestAdaptiveClassifierTrainPersistStatusAndUnloadRoutes(t *testing.T) {
	server, _, _, dataDir := testServerWithData(t, "adaptive-test-token", false)
	recordingsDir := filepath.Join(dataDir, "recordings")
	for _, item := range []struct {
		name     string
		variance float64
		amp      []float64
	}{
		{name: "train_absent_empty-room.jsonl", variance: 0.1, amp: []float64{1, 1, 1, 1}},
		{name: "train_walking_room.jsonl", variance: 10, amp: []float64{2, 6, 4, 9}},
	} {
		file, err := os.Create(filepath.Join(recordingsDir, item.name))
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for i := 0; i < 6; i++ {
			frame := map[string]any{
				"features": map[string]any{"variance": item.variance, "motion_band_power": item.variance * 2, "mean_rssi": -55},
				"nodes":    []any{map[string]any{"node_id": 1, "amplitude": item.amp}},
			}
			if err := encoder.Encode(frame); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	unauthorized, err := http.Post(server.URL+"/api/v1/adaptive/train", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("adaptive training without a token returned %d", unauthorized.StatusCode)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/adaptive/train", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer adaptive-test-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || result["success"] != true || result["accuracy_kind"] != "in_sample" || result["persisted"] != true || result["trained_frames"] != float64(12) {
		t.Fatalf("training result status=%d body=%v error=%v", response.StatusCode, result, decodeErr)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "adaptive_model.json")); err != nil {
		t.Fatalf("trained model was not persisted: %v", err)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/v1/adaptive/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer adaptive-test-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	decodeErr = json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || status["loaded"] != true || status["accuracy_kind"] != "in_sample" {
		t.Fatalf("adaptive status=%d body=%v error=%v", response.StatusCode, status, decodeErr)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/api/v1/adaptive/unload", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer adaptive-test-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("adaptive unload status=%d", response.StatusCode)
	}
	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/v1/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer adaptive-test-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Features map[string]any `json:"features"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&info)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || info.Features["adaptive_activity_training"] != true || info.Features["adaptive_activity_model_loaded"] != false {
		t.Fatalf("adaptive feature flags status=%d body=%v error=%v", response.StatusCode, info, decodeErr)
	}
	loaded, err := adaptive.Load(filepath.Join(dataDir, "adaptive_model.json"))
	if err != nil || loaded.TrainedFrames != 12 {
		t.Fatalf("unload removed persisted model: model=%+v err=%v", loaded, err)
	}
}

func TestVendorCSILatestRoutesExposeOnlyAcceptedSnapshots(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	mean, peak := float32(9), float32(13)
	store.IngestVendorCSI(csivendor.Snapshot{
		EventType: "mediatek_csi", Source: "mediatek:physical-unvalidated", ReportKind: "csi",
		Sequence: 17, TimestampUS: 42, DeviceID: "0000000000001234", Chipset: "mt7981-mt7976",
		BandwidthMHz: 20, TXCount: 1, RXCount: 1, SubcarrierCount: 2, ElementCount: 2,
		RSSIDBm: []int8{-45}, MeanAmplitude: &mean, PeakAmplitude: &peak,
	})
	response, err := http.Get(server.URL + "/api/v1/csi/mediatek/latest")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || body["source"] != "mediatek:physical-unvalidated" || body["event_type"] != "mediatek_csi" {
		t.Fatalf("MediaTek latest response status=%d body=%v err=%v", response.StatusCode, body, decodeErr)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/qualcomm/latest")
	if err != nil {
		t.Fatal(err)
	}
	var empty map[string]any
	decodeErr = json.NewDecoder(response.Body).Decode(&empty)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || empty["status"] != "no Qualcomm CSI data yet" {
		t.Fatalf("empty Qualcomm route status=%d body=%v err=%v", response.StatusCode, empty, decodeErr)
	}
}

func TestMeshSyncEndpointsExposeDecodedESP32State(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	store.IngestSync(protocol.SyncPacket{
		NodeID: 9, ProtoVer: protocol.SyncProtocolVersion,
		Flags:   protocol.SyncFlags{IsValid: true, SmoothedUsed: true},
		LocalUS: 28_798_450, EpochUS: 27_634_885, Sequence: 20,
	})
	store.IngestCSI(protocol.CSIFrame{
		NodeID: 9, Antennas: 1, Subcarriers: 2, Sequence: 20, RSSIDBm: -50, NoiseFloor: -90,
		Flags:     protocol.CSIFlagMeshSyncValid,
		Amplitude: []float64{12, 13}, Phase: []float64{0.1, 0.2},
	})
	store.IngestVitals(protocol.VitalsPacket{NodeID: 9, Presence: true, Persons: 2, CountValid: true, RSSIDBm: -49})
	store.IngestVitals(protocol.VitalsPacket{NodeID: 10}) // tracked node, but no sync packet or CSI
	infoResponse, err := http.Get(server.URL + "/api/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Features map[string]bool `json:"features"`
	}
	decodeErr := json.NewDecoder(infoResponse.Body).Decode(&info)
	infoResponse.Body.Close()
	if decodeErr != nil || infoResponse.StatusCode != http.StatusOK || !info.Features["mesh_sync"] || !info.Features["vendor_rf_adapters"] || !info.Features["vendor_csi_adapters"] || !info.Features["room_classification_fusion"] || !info.Features["mesh_time_fusion"] || !info.Features["spatial_csi_fusion"] {
		t.Fatalf("mesh capability flags are inaccurate: status=%d features=%v error=%v", infoResponse.StatusCode, info.Features, decodeErr)
	}

	spatialResponse, err := http.Get(server.URL + "/api/v1/spatial-fusion/status")
	if err != nil {
		t.Fatal(err)
	}
	var spatial struct {
		StreamStatus string `json:"stream_status"`
		Fusion       struct {
			Status                       string `json:"status"`
			ActiveNodes                  int    `json:"active_nodes"`
			TimeAlignment                string `json:"time_alignment"`
			Synchronized                 bool   `json:"synchronized"`
			SpatialLocalizationAvailable bool   `json:"spatial_localization_available"`
		} `json:"fusion"`
	}
	decodeErr = json.NewDecoder(spatialResponse.Body).Decode(&spatial)
	spatialResponse.Body.Close()
	if decodeErr != nil || spatialResponse.StatusCode != http.StatusOK || spatial.StreamStatus != "fresh" || spatial.Fusion.Status != "single_node" || spatial.Fusion.ActiveNodes != 1 || spatial.Fusion.TimeAlignment != "mesh_epoch" || !spatial.Fusion.Synchronized || spatial.Fusion.SpatialLocalizationAvailable {
		t.Fatalf("spatial fusion status=%d body=%+v error=%v", spatialResponse.StatusCode, spatial, decodeErr)
	}

	response, err := http.Get(server.URL + "/api/v1/mesh")
	if err != nil {
		t.Fatal(err)
	}
	var mesh struct {
		Nodes map[string]state.SyncSnapshot `json:"nodes"`
		Total int                           `json:"total"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&mesh)
	response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.StatusCode != http.StatusOK || mesh.Total != 1 || mesh.Nodes["9"].OffsetUS != 1_163_565 {
		t.Fatalf("mesh endpoint status=%d body=%+v", response.StatusCode, mesh)
	}

	response, err = http.Get(server.URL + "/api/v1/nodes/9/sync")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot state.SyncSnapshot
	decodeErr = json.NewDecoder(response.Body).Decode(&snapshot)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || snapshot.Sequence != 20 {
		t.Fatalf("node sync status=%d snapshot=%+v error=%v", response.StatusCode, snapshot, decodeErr)
	}

	for _, tc := range []struct {
		path string
		code string
	}{{"/api/v1/nodes/10/sync", "no_sync"}, {"/api/v1/nodes/99/sync", "unknown_node"}} {
		response, err = http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		decodeErr = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusNotFound || body["error"] != tc.code {
			t.Fatalf("%s status=%d body=%v error=%v", tc.path, response.StatusCode, body, decodeErr)
		}
	}

	response, err = http.Get(server.URL + "/api/v1/nodes/300/sync")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-range node id returned status %d", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/api/v1/mesh/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, marker := range []string{
		"wifi_densepose_mesh_node_total{state=\"leader\"} 0",
		"wifi_densepose_mesh_node_total{state=\"follower\"} 1",
		"wifi_densepose_mesh_node_total{state=\"no_sync\"} 1",
		"wifi_densepose_mesh_offset_us{node=\"9\"} 1163565",
	} {
		if !strings.Contains(string(metrics), marker) {
			t.Fatalf("mesh metrics missing %q:\n%s", marker, metrics)
		}
	}

	response, err = http.Get(server.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	var nodeList struct {
		Nodes []state.NodeStatus `json:"nodes"`
		Total int                `json:"total"`
	}
	decodeErr = json.NewDecoder(response.Body).Decode(&nodeList)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || nodeList.Total != 2 || len(nodeList.Nodes) != 2 {
		t.Fatalf("node status list status=%d body=%+v error=%v", response.StatusCode, nodeList, decodeErr)
	}
	live, syncOnly := nodeList.Nodes[0], nodeList.Nodes[1]
	if live.NodeID != 9 || live.Status != "active" || live.CSIStatus != "active" || live.CSISequence == nil || *live.CSISequence != 20 || live.PersonCount != 2 || live.PersonCountValid == nil || !*live.PersonCountValid || live.PositionConfigured {
		t.Fatalf("live node status does not match the sensing state: %+v", live)
	}
	if syncOnly.NodeID != 10 || syncOnly.Status != "stale" || syncOnly.LastSeenMS != 999999 || syncOnly.CSISequence != nil || syncOnly.CSILastSeenMS != nil {
		t.Fatalf("sync-only node status incorrectly suggests CSI activity: %+v", syncOnly)
	}
}

func TestEdgeRegistryEndpointCachesRefreshesAndServesStaleCopy(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = io.WriteString(w, `{"version":1}`)
			return
		}
		http.Error(w, "upstream detail must not reach clients", http.StatusBadGateway)
	}))
	defer upstream.Close()

	cfg := config.Config{
		BindAddr: "127.0.0.1", HTTPPort: 8080, WSPort: 8765,
		UDPBindAddr: "127.0.0.1", UDPPort: 5005, Source: "auto",
		UIPath: t.TempDir(), EdgeRegistryURL: upstream.URL, EdgeRegistryTTL: 3600,
	}
	hosts, err := security.NewHostAllowlist([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	api := New(cfg, state.New("auto", nil), nil, hosts, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	for _, tc := range []struct {
		path  string
		stale bool
	}{
		{path: "/api/v1/edge/registry", stale: false},
		{path: "/api/v1/edge/registry?refresh=1", stale: true},
	} {
		response, requestErr := http.Get(server.URL + tc.path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		var body struct {
			Stale    bool `json:"stale"`
			Registry struct {
				Version int `json:"version"`
			} `json:"registry"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if response.StatusCode != http.StatusOK || body.Stale != tc.stale || body.Registry.Version != 1 {
			t.Fatalf("%s: status=%d stale=%v registry=%+v", tc.path, response.StatusCode, body.Stale, body.Registry)
		}
	}
	if requests != 2 {
		t.Fatalf("unexpected upstream request count: %d", requests)
	}
}

func TestEdgeRegistryDisabledReturnsNotFound(t *testing.T) {
	server, _, _ := testServer(t, "", false)
	response, err := http.Get(server.URL + "/api/v1/edge/registry")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `"code":"edge_registry_disabled"`) {
		t.Fatalf("disabled edge registry response=%d %s", response.StatusCode, body)
	}
}

func TestMediaTekDeviceListingAndUnavailableHistoryRoutes(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	store.IngestVendorCSI(csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek", DeviceID: "node-b", Sequence: 2})
	store.IngestVendorCSI(csivendor.Snapshot{
		EventType: "mediatek_csi", Source: "mediatek", DeviceID: "node-a", Sequence: 1,
		Frame: &csivendor.CSIFrame{TimestampUS: 123, Sequence: 1, RSSIDBm: []int8{-45}, SubcarrierCount: 2,
			Chains: []csivendor.Chain{{TX: 0, RX: 1, Amplitude: []float32{5, 13}, Phase: []float32{.9, 1.1}}}},
	})
	response, err := http.Get(server.URL + "/api/v1/csi/mediatek/devices")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Total   int                      `json:"total"`
		Devices []map[string]interface{} `json:"devices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || listed.Total != 2 || len(listed.Devices) != 2 || listed.Devices[0]["device_id"] != "node-a" || listed.Devices[1]["device_id"] != "node-b" || listed.Devices[0]["age_ms"] == nil {
		t.Fatalf("MediaTek device list mismatch: status=%d body=%+v", response.StatusCode, listed)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/devices/node-a/frames?n=1&fields=amp")
	if err != nil {
		t.Fatal(err)
	}
	var frameResult map[string]any
	if err := json.NewDecoder(response.Body).Decode(&frameResult); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	frameRows, ok := frameResult["frames"].([]any)
	if response.StatusCode != http.StatusOK || !ok || len(frameRows) != 1 {
		t.Fatalf("MediaTek frame route mismatch: status=%d body=%v", response.StatusCode, frameResult)
	}
	row, ok := frameRows[0].(map[string]any)
	if !ok || row["seq"] != float64(1) || row["phase"] != nil || row["amp"] == nil || row["rssi"] == nil {
		t.Fatalf("MediaTek frame selection/query shape mismatch: %v", frameRows[0])
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/devices/node-a/summary")
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.NewDecoder(response.Body).Decode(&summary); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	chains, ok := summary["chains"].([]any)
	if response.StatusCode != http.StatusOK || summary["ring_frame_count"] != float64(1) || !ok || len(chains) != 1 || summary["heuristic"] != nil {
		t.Fatalf("MediaTek summary response mismatch: status=%d body=%v", response.StatusCode, summary)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/devices/missing/frames")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown MediaTek frame device status=%d, want 404", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/devices/missing/summary")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown MediaTek summary device status=%d, want 404", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/devices/node-a/frames?n=-1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid MediaTek frame count status=%d, want 400", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/activity")
	if err != nil {
		t.Fatal(err)
	}
	var activityBody struct {
		SampleCount int `json:"sample_count"`
		Samples     []struct {
			Devices map[string]map[string]float64 `json:"devices"`
		} `json:"samples"`
	}
	if err := json.NewDecoder(response.Body).Decode(&activityBody); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	deviceActivity, hasDevice := map[string]float64{}, false
	if len(activityBody.Samples) == 1 {
		deviceActivity, hasDevice = activityBody.Samples[0].Devices["node-a"]
	}
	_, hasRelativeLevel := deviceActivity["level"]
	_, hasAbsoluteLevel := deviceActivity["abs_level"]
	if response.StatusCode != http.StatusOK || activityBody.SampleCount != 1 || len(activityBody.Samples) != 1 || !hasDevice || !hasRelativeLevel || !hasAbsoluteLevel {
		t.Fatalf("MediaTek activity history did not expose its real bounded sample: status=%d body=%+v", response.StatusCode, activityBody)
	}
	response, err = http.Get(server.URL + "/api/v1/csi/mediatek/activity?seconds=bad")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid activity window status=%d, want 400", response.StatusCode)
	}
	for _, tc := range []struct {
		path       string
		statusCode int
		marker     string
	}{
		{path: "/api/v1/csi/mediatek/activity?seconds=900", statusCode: http.StatusOK, marker: `"seconds":300`},
	} {
		response, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != tc.statusCode || !strings.Contains(string(body), tc.marker) {
			t.Errorf("%s: status=%d body=%s", tc.path, response.StatusCode, body)
		}
	}
}

func TestRealtekRadarLatestRouteServesAggregateOnly(t *testing.T) {
	server, store, _ := testServer(t, "", false)
	response, err := http.Get(server.URL + "/api/v1/radar/latest")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || body["status"] != "no Realtek radar data yet" {
		t.Fatalf("no-data response mismatch: status=%d body=%v", response.StatusCode, body)
	}
	peakRange, peakPower := float32(2.5), float32(9)
	store.IngestRadar(radar.Snapshot{
		EventType: "realtek_radar", Source: "realtek:simulated", ReportType: "range_near",
		PeakRangeM: &peakRange, PeakPower: &peakPower,
	})
	response, err = http.Get(server.URL + "/api/v1/radar/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body["source"] != "realtek:simulated" || body["peak_range_m"] != float64(2.5) || body["peak_power"] != float64(9) || body["payload"] != nil {
		t.Fatalf("radar summary response mismatch: status=%d body=%v", response.StatusCode, body)
	}
}

func TestRuFieldEndpointsAreExplicitAndAuthGated(t *testing.T) {
	server, _, _ := testServer(t, "rufield-token", false)

	response, err := http.Get(server.URL + "/api/field")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized RuField API status=%d", response.StatusCode)
	}

	for _, path := range []string{"/api/field", "/ws/field"} {
		request, requestErr := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", "Bearer rufield-token")
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), `"code":"rufield_unavailable"`) {
			t.Fatalf("%s should report the missing port explicitly: status=%d body=%s", path, response.StatusCode, body)
		}
	}

	response, err = http.Get(server.URL + "/ws/field")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized RuField WebSocket status=%d", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/ws/field", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer rufield-token")
	request.Header.Set("Origin", "https://attacker.example")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `"code":"origin_rejected"`) {
		t.Fatalf("untrusted RuField WebSocket origin status=%d body=%s", response.StatusCode, body)
	}
}

func TestFieldHotspotsAreNotReportedAsPeople(t *testing.T) {
	values := make([]float64, 20*20)
	values[4*20+15] = 1
	update := state.SensingUpdate{
		Classification: signal.Classification{Presence: true},
		SignalField:    signal.SignalField{GridSize: [3]int{20, 1, 20}, Values: values},
	}
	hotspots := fieldHotspots(update)
	if len(hotspots) != 1 || hotspots[0].Cell != [2]int{15, 4} {
		t.Fatalf("unexpected signal hotspots: %+v", hotspots)
	}
	update.Classification.Presence = false
	if got := fieldHotspots(update); len(got) != 0 {
		t.Fatalf("absent signal must not create a hotspot: %+v", got)
	}
}

func TestHandlerPanicReturnsGenericInternalError(t *testing.T) {
	handler := observeRequestsMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("sensitive request detail")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "sensitive request detail") {
		t.Fatalf("panic was not contained safely: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("generic error envelope missing: %s", response.Body.String())
	}
}

func TestBearerAuthAndSingleUseWebSocketTicket(t *testing.T) {
	server, store, hub := testServer(t, "test-secret", false)
	resp, err := http.Get(server.URL + "/api/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized read status=%d", resp.StatusCode)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/ws-ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-secret")
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var ticketBody struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ticketBody); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(ticketBody.Ticket) != 64 {
		t.Fatalf("ticket response status=%d ticket length=%d", resp.StatusCode, len(ticketBody.Ticket))
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/sensing?ticket=" + ticketBody.Ticket
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test complete")
	deadline := time.Now().Add(time.Second)
	for hub.ClientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	store.IngestVitals(protocol.VitalsPacket{NodeID: 2, Presence: true, PresenceScore: 0.8, Persons: 1, CountValid: true})
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message), `"type":"sensing_update"`) {
		t.Fatalf("unexpected stream message: %s", message)
	}

	_, response, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("replayed WebSocket ticket was accepted")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay response=%v err=%v", response, err)
	}
}

func TestPrivacyModeRedactsWebSocketFrames(t *testing.T) {
	server, store, hub := testServer(t, "privacy-token", true)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/ws-ticket", nil)
	request.Header.Set("Authorization", "Bearer privacy-token")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/sensing?ticket=" + result.Ticket
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test complete")
	deadline := time.Now().Add(time.Second)
	for hub.ClientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	store.IngestFeatureState(protocol.FeatureStatePacket{
		NodeID: 1, PresenceScore: 0.9, MotionScore: 0.2, RespirationBPM: 14,
		RespirationConf: 0.8, HeartbeatBPM: 72, HeartbeatConf: 0.8, NodeCoherence: 0.9,
		QualityFlags: 1 | 2 | 4,
	})
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(message), "vital_signs") || strings.Contains(string(message), "heart_rate_bpm") || strings.Contains(string(message), "breathing_rate_bpm") {
		t.Fatalf("privacy-filtered stream leaked biometric fields: %s", message)
	}
	if !strings.Contains(string(message), `"presence":true`) {
		t.Fatalf("privacy filter removed non-biometric presence: %s", message)
	}
	store.IngestEdgeFusedVitals(protocol.EdgeFusedVitalsPacket{
		NodeID: 2, Flags: 1, BreathingRateBPM: 14, HeartRateBPM: 72,
		MmwaveHeartRateBPM: 71, MmwaveBreathingBPM: 15, PersonCountValid: true, Persons: 1,
	})
	_, message, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(message), "heartrate_bpm") || strings.Contains(string(message), "breathing_rate_bpm") || strings.Contains(string(message), "hr_bpm") || strings.Contains(string(message), "br_bpm") {
		t.Fatalf("privacy-filtered edge event leaked biometric fields: %s", message)
	}
	if !strings.Contains(string(message), `"type":"sensing_update"`) {
		t.Fatalf("unexpected first frame after edge vitals: %s", message)
	}
	_, message, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(message), "heartrate_bpm") || strings.Contains(string(message), "breathing_rate_bpm") || strings.Contains(string(message), "hr_bpm") || strings.Contains(string(message), "br_bpm") {
		t.Fatalf("privacy-filtered edge event leaked biometric fields: %s", message)
	}
	if !strings.Contains(string(message), `"type":"edge_fused_vitals"`) || !strings.Contains(string(message), `"presence_score"`) {
		t.Fatalf("privacy filter removed non-biometric edge event fields: %s", message)
	}
}

func TestModelRESTLifecycle(t *testing.T) {
	modelDir := t.TempDir()
	models, err := model.NewRegistry(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub()
	store := state.New("auto", hub)
	hosts, _ := security.NewHostAllowlist(nil)
	cfg := config.Config{BindAddr: "127.0.0.1", HTTPPort: 8080, WSPort: 8765, UDPBindAddr: "127.0.0.1", UDPPort: 5005, Source: "auto", UIPath: t.TempDir(), DataDir: t.TempDir()}
	recorder, err := recording.New(cfg.DataDir, hub, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(New(cfg, store, security.NewTicketStore(), hosts, models, recorder, logger).Handler())
	t.Cleanup(server.Close)

	manifest, _ := json.Marshal(map[string]string{"model_id": "test", "version": "1.0.0", "description": "fixture"})
	weights := make([]byte, 4)
	binary.LittleEndian.PutUint32(weights, math.Float32bits(0.5))
	data := model.BuildRVF(
		model.Segment{Type: 5, Payload: manifest},
		model.Segment{Type: 1, Payload: weights},
	)
	if err := os.WriteFile(filepath.Join(modelDir, "fixture.rvf"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	response, err := http.Get(server.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Models []model.ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(listing.Models) != 1 || !listing.Models[0].Loadable {
		t.Fatalf("status=%d models=%+v", response.StatusCode, listing.Models)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/models/load", strings.NewReader(`{"model_id":"fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("model load status=%d", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/models/active")
	if err != nil {
		t.Fatal(err)
	}
	var active model.ActiveModel
	if err := json.NewDecoder(response.Body).Decode(&active); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if active.ModelID != "fixture" || active.InferenceAvailable {
		t.Fatalf("unexpected active model status: %+v", active)
	}
}

func TestRecordingRESTLifecycle(t *testing.T) {
	server, _, hub := testServer(t, "", false)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/recording/start", strings.NewReader(`{"id":"api-test","label":"ignored by legacy request"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recording start status=%d", response.StatusCode)
	}
	hub.Publish([]byte(`{"type":"sensing_update","source":"simulate","presence":false}`))
	response, err = http.Post(server.URL+"/api/v1/recording/stop", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recording stop status=%d", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/recording/list")
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Recordings []map[string]any `json:"recordings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(listing.Recordings) != 1 || listing.Recordings[0]["id"] != "api-test" {
		t.Fatalf("recordings=%+v", listing.Recordings)
	}
}
