package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/state"
)

func TestSpaceLayoutPersistenceRevisionAndSensorBinding(t *testing.T) {
	server, store, _, dir, _ := testServerWithData(t, "", false)
	defer server.Close()
	body := `{"revision":0,"devices":[{"id":"rx2","kind":"esp32","node_id":2,"label":"Window","position":[-2,-3,1]},{"id":"ap","kind":"router","label":"Router","position":[0,0,0]}]}`
	resp, err := http.Post(server.URL+"/api/v1/space/layout", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	store.IngestCSI(protocol.CSIFrame{NodeID: 2, Antennas: 1, Subcarriers: 2, Amplitude: []float64{1, 2}, Source: "esp32"})
	nodes := store.SnapshotNodes()
	if len(nodes) != 1 || !nodes[0].PositionAvailable || nodes[0].Position != [3]float64{-2, -3, 1} {
		t.Fatalf("geometry not bound by node ID: %+v", nodes)
	}
	restored := &Server{Config: config.Config{DataDir: dir}, State: state.New("esp32", nil)}
	restored.initSpace()
	if restored.spaceErr != nil || restored.spaceLayout.Revision != 1 || len(restored.spaceLayout.Devices) != 2 {
		t.Fatalf("restore: %+v %v", restored.spaceLayout, restored.spaceErr)
	}
	resp2, err := http.Post(server.URL+"/api/v1/space/layout", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 409 {
		t.Fatalf("stale update accepted: %d", resp2.StatusCode)
	}
	if restored.spaceLayout.Devices[0].Position[1] != -3 {
		t.Fatal("negative vertical coordinate lost")
	}
}

func TestSpaceRequiresAuthorizationAndRejectsMalformedLayouts(t *testing.T) {
	server, _, _ := testServer(t, "space-secret", false)
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/space/layout")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unauthorized layout exposed: %d", response.StatusCode)
	}
	bodies := []string{
		`{}`, `{"devices":null}`,
		`{"devices":[{"id":"x","kind":"human","label":"Person","position":[0,0,0]}]}`,
		`{"devices":[{"id":"x","kind":"router","label":"Router","position":[1,2]}]}`,
		`{"devices":[{"id":"x","kind":"router","label":"Router","position":[1,2,3,4]}]}`,
		`{"devices":[{"id":"x","kind":"esp32","label":"Node","position":[0,0,0]}]}`,
		`{"devices":[{"id":"x","kind":"router","label":"Router","position":[10001,0,0]}]}`,
		`{"devices":[],"tracks":[{"x":1}]}`,
	}
	for _, body := range bodies {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/space/layout", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer space-secret")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Errorf("invalid layout status=%d body=%s", response.StatusCode, body)
		}
	}
}

func TestSpaceRejectsDuplicateAndNonfinitePositions(t *testing.T) {
	id := uint8(7)
	device := spaceDevice{ID: "a", Kind: "esp32", NodeID: &id, Label: "Node", Position: []float64{0, 0, 0}}
	if err := validateSpace(spaceLayout{Devices: []spaceDevice{device, device}}); err == nil {
		t.Fatal("duplicate accepted")
	}
	duplicate := device
	duplicate.ID = "b"
	if err := validateSpace(spaceLayout{Devices: []spaceDevice{device, duplicate}}); err == nil {
		t.Fatal("duplicate node binding accepted")
	}
	device.Position[0] = math.Inf(1)
	if err := validateSpace(spaceLayout{Devices: []spaceDevice{device}}); err == nil {
		t.Fatal("infinite coordinate accepted")
	}
}

func TestSpaceEmptyStateDoesNotClaimDetectedObjects(t *testing.T) {
	s := &Server{Config: config.Config{DataDir: t.TempDir()}, State: state.New("esp32", nil)}
	s.initSpace()
	recorder := httptest.NewRecorder()
	s.getSpace(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["person_localization_available"] != false || body["animal_detection_available"] != false || body["position_source"] != "operator_configured" {
		t.Fatalf("unsupported claims: %v", body)
	}
	if health := s.State.Health(); health["frames_received"].(uint64) != 0 {
		t.Fatal("layout generated measurements")
	}
	if nodes := s.State.NodeStatuses(time.Now()); len(nodes) != 0 {
		t.Fatal("configured devices impersonate connected devices")
	}
}

func TestSpaceCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "space-layout.json"), []byte(`{"devices":[{"id":"bad"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: config.Config{DataDir: dir}, State: state.New("esp32", nil)}
	s.initSpace()
	recorder := httptest.NewRecorder()
	s.getSpace(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != 503 {
		t.Fatalf("corrupt layout accepted: %d", recorder.Code)
	}
}
