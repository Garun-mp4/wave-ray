package state

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/calibration"
	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/inference"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/signal"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func TestIngestCSIProducesFreshCompatibleUpdate(t *testing.T) {
	hub := ws.NewHub()
	sub := hub.Subscribe()
	store := New("auto", hub)
	store.IngestCSI(protocol.CSIFrame{
		NodeID: 4, Subcarriers: 4, RSSIDBm: -51, NoiseFloor: -90,
		Amplitude: []float64{10, 12, 13, 11}, Phase: []float64{0, 0.2, 0.3, 0.1},
	})
	update, status, age := store.Latest(time.Now())
	if status != "fresh" || age < 0 || update.Type != "sensing_update" || update.Source != "esp32" {
		t.Fatalf("unexpected update state: status=%s age=%s update=%+v", status, age, update)
	}
	if len(update.Nodes) != 1 || update.Nodes[0].NodeID != 4 || update.Nodes[0].RSSIDBm != -51 {
		t.Fatalf("node shape mismatch: %+v", update.Nodes)
	}
	if update.Nodes[0].PositionAvailable {
		t.Fatal("unconfigured node coordinates were reported as available")
	}
	var streamed map[string]any
	if err := json.Unmarshal(<-sub.C, &streamed); err != nil {
		t.Fatal(err)
	}
	if streamed["type"] != "sensing_update" {
		t.Fatalf("unexpected stream event: %+v", streamed)
	}
}

func TestLoadedRVFModelParticipatesInLiveCSIInference(t *testing.T) {
	modelDir := t.TempDir()
	registry, err := model.NewRegistry(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	weights := [][]float64{make([]float64, adaptive.FeatureCount+1), make([]float64, adaptive.FeatureCount+1)}
	weights[0][adaptive.FeatureCount] = -8
	weights[1][adaptive.FeatureCount] = 8
	std := [adaptive.FeatureCount]float64{}
	for i := range std {
		std[i] = 1
	}
	trained := &adaptive.Model{
		ClassNames: []string{"absent", "present_moving"},
		ClassStats: []adaptive.ClassStats{{Label: "absent", Count: 2}, {Label: "present_moving", Count: 2}},
		GlobalStd:  std, Weights: weights, TrainedFrames: 4, TrainingAccuracy: 1,
		Version: adaptive.TrainedModelVersion,
	}
	artifact, err := model.BuildActivityRVF("activity", trained)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.InstallGenerated("activity", artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Load("activity"); err != nil {
		t.Fatal(err)
	}
	store := New("esp32", nil)
	store.SetModelRegistry(registry)
	amplitude := make([]float64, 56)
	for i := range amplitude {
		amplitude[i] = float64(i%7 + 1)
	}
	store.IngestCSI(protocol.CSIFrame{NodeID: 3, Sequence: 1, Subcarriers: 56, RSSIDBm: -50, NoiseFloor: -90, Amplitude: amplitude})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || len(update.NodeFeatures) != 1 || update.NodeFeatures[0].Classification.MotionLevel != "present_moving" {
		t.Fatalf("loaded RVF weights did not change live CSI classification: status=%s features=%+v", status, update.NodeFeatures)
	}
	active, ok := registry.Active()
	if !ok || active.FramesProcessed != 1 {
		t.Fatalf("RVF inference counter not incremented: active=%+v loaded=%v", active, ok)
	}
}

func TestSpatialFusionUsesMeshClockAndNodeIdentityPositions(t *testing.T) {
	store := New("esp32", ws.NewHub())
	if err := store.SetNodePositions(map[uint8][3]float64{
		9: {4, 5, 6},
		3: {1, 2, 3},
	}); err != nil {
		t.Fatal(err)
	}
	for _, node := range []struct {
		id    uint8
		epoch uint64
		amp   float64
	}{{9, 110_000, 20}, {3, 100_000, 10}} {
		store.IngestSync(protocol.SyncPacket{
			NodeID: node.id, ProtoVer: protocol.SyncProtocolVersion,
			Flags: protocol.SyncFlags{IsValid: true}, EpochUS: node.epoch, Sequence: 40,
		})
		amplitude, phase := make([]float64, 56), make([]float64, 56)
		for i := range amplitude {
			amplitude[i], phase[i] = node.amp, 0.1
		}
		store.IngestCSI(protocol.CSIFrame{
			NodeID: node.id, Antennas: 1, Subcarriers: 56, Sequence: 40,
			Flags: protocol.CSIFlagMeshSyncValid, RSSIDBm: -50, NoiseFloor: -90,
			Amplitude: amplitude, Phase: phase,
		})
	}

	update, status, _ := store.Latest(time.Now())
	if status != "fresh" {
		t.Fatalf("latest sensing status=%q", status)
	}
	fusion := update.SpatialFusion
	if fusion.Status != "fused" || fusion.ActiveNodes != 2 || fusion.TimeAlignment != "mesh_epoch" || !fusion.Synchronized {
		t.Fatalf("unexpected spatial fusion result: %+v", fusion)
	}
	if fusion.TimestampSpreadUS != 10_000 || fusion.PhaseFusion.Reason != "independent_clocks" || fusion.SpatialLocalizationAvailable {
		t.Fatalf("fusion timing/phase/localization claims are wrong: %+v", fusion)
	}
	if len(fusion.FusedAmplitude) != 56 || fusion.FusedAmplitude[0] < 14.9 || fusion.FusedAmplitude[0] > 15.1 {
		t.Fatalf("unexpected fused CSI amplitude vector length=%d first=%f", len(fusion.FusedAmplitude), fusion.FusedAmplitude[0])
	}
	if len(update.Nodes) != 2 || !update.Nodes[0].PositionAvailable || update.Nodes[0].NodeID != 3 || update.Nodes[0].Position != [3]float64{1, 2, 3} || update.Nodes[1].NodeID != 9 || update.Nodes[1].Position != [3]float64{4, 5, 6} {
		t.Fatalf("node geometry was not attached by identity: %+v", update.Nodes)
	}
	if statuses := store.NodeStatuses(time.Now()); len(statuses) != 2 || !statuses[0].PositionConfigured || !statuses[1].PositionConfigured {
		t.Fatalf("node position configuration status=%+v", statuses)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	spatial, ok := payload["spatial_fusion"].(map[string]any)
	if !ok {
		t.Fatalf("spatial fusion missing from serialized update: %s", encoded)
	}
	ids, ok := spatial["contributor_node_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != float64(3) || ids[1] != float64(9) {
		t.Fatalf("contributor IDs must serialize as JSON numbers: %s", encoded)
	}
}

func TestSpatialFusionDoesNotTreatHostWiFiAsCSI(t *testing.T) {
	store := New("wifi", ws.NewHub())
	store.IngestCSI(protocol.CSIFrame{
		Source: "wifi", InputMode: "rssi_derived", NodeID: 1, Subcarriers: 56,
		Amplitude: make([]float64, 56),
	})
	update, _, _ := store.Latest(time.Now())
	if update.SpatialFusion.Available || update.SpatialFusion.Status != "unavailable" {
		t.Fatalf("RSSI-derived input was used as multi-sensor CSI: %+v", update.SpatialFusion)
	}
}

func TestUntrustedEdgeBiometricsAreNeverPublishedToWebSocket(t *testing.T) {
	hub := ws.NewHub()
	subscription := hub.Subscribe()
	store := New("esp32", hub)
	store.IngestEdgeFusedVitals(protocol.EdgeFusedVitalsPacket{
		NodeID: 3, Flags: 1, BreathingRateBPM: 14, HeartRateBPM: 72,
		MmwaveHeartRateBPM: 71, MmwaveBreathingBPM: 15, MmwaveDistanceCM: 190,
		Persons: 1, PersonCountValid: true, FusionConfidence: 240,
	})
	_ = <-subscription.C // sensing_update
	select {
	case encoded := <-subscription.C:
		for _, field := range []string{"heartrate_bpm", "breathing_rate_bpm", "hr_bpm", "br_bpm", "distance_cm"} {
			if strings.Contains(string(encoded), field) {
				t.Fatalf("untrusted edge measurement %q leaked to WebSocket: %s", field, encoded)
			}
		}
		if !strings.Contains(string(encoded), `"biometric_authority":"abstained"`) || !strings.Contains(string(encoded), `"biometrics_authorized":false`) {
			t.Fatalf("edge telemetry did not communicate the trust decision: %s", encoded)
		}
	case <-time.After(time.Second):
		t.Fatal("edge telemetry WebSocket event was not published")
	}
}

func TestGroundTruthDedupFactorAppliesToFreshNodeCountFallback(t *testing.T) {
	manager, err := calibration.New(filepath.Join(t.TempDir(), "room_calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetDedupFactor(1); err != nil {
		t.Fatal(err)
	}
	store := New("esp32", ws.NewHub())
	store.SetCalibrationManager(manager)
	store.roomDebouncedLevel = "present_still"
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true, Persons: 1, CountValid: true})
	store.IngestVitals(protocol.VitalsPacket{NodeID: 2, Presence: true, Persons: 1, CountValid: true})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.EstimatedPersons == nil || *update.EstimatedPersons != 2 {
		t.Fatalf("dedup factor was not applied to two fresh single-person node counts: status=%s update=%+v", status, update)
	}
	if update.MeasurementAuthority != "unverified" || update.CalibratedEvidenceAuthorized || update.NumericVitalsAuthorized {
		t.Fatalf("count fusion incorrectly promoted heuristic counts to trusted measurements: %+v", update)
	}
}

func TestAdaptiveClassifierOverridesOnlyCSIHeuristicAndCanUnload(t *testing.T) {
	store := New("esp32", ws.NewHub())
	weights := [][]float64{make([]float64, adaptive.FeatureCount+1), make([]float64, adaptive.FeatureCount+1)}
	weights[0][adaptive.FeatureCount] = -2
	weights[1][adaptive.FeatureCount] = 2
	model := &adaptive.Model{
		ClassStats: []adaptive.ClassStats{{Label: "absent", Count: 1}, {Label: "learned_motion", Count: 1}},
		Weights:    weights, GlobalStd: [adaptive.FeatureCount]float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		TrainedFrames: 2, TrainingAccuracy: 1, Version: adaptive.TrainedModelVersion,
		ClassNames: []string{"absent", "learned_motion"},
	}
	if err := store.SetAdaptiveModel(model); err != nil {
		t.Fatal(err)
	}
	model.Weights[1][adaptive.FeatureCount] = -100
	store.IngestCSI(protocol.CSIFrame{NodeID: 2, Subcarriers: 2, RSSIDBm: -50, Amplitude: []float64{1, 2}, Phase: []float64{0, 0}})
	update, _, _ := store.Latest(time.Now())
	if update.NodeFeatures[0].Classification.MotionLevel != "learned_motion" || !update.NodeFeatures[0].Classification.Presence {
		t.Fatalf("loaded adaptive classifier was not applied: %+v", update.NodeFeatures[0].Classification)
	}
	if err := store.SetAdaptiveModel(nil); err != nil {
		t.Fatal(err)
	}
	store.IngestCSI(protocol.CSIFrame{NodeID: 2, Sequence: 2, Subcarriers: 2, RSSIDBm: -50, Amplitude: []float64{1, 2}, Phase: []float64{0, 0}})
	update, _, _ = store.Latest(time.Now())
	if update.NodeFeatures[0].Classification.MotionLevel == "learned_motion" {
		t.Fatalf("unloaded model continued to classify: %+v", update.NodeFeatures[0].Classification)
	}
}

func TestVendorCSISnapshotsStaySeparateAndPreserveSourcePriority(t *testing.T) {
	store := New("auto", ws.NewHub())
	mean, peak := float32(9), float32(13)
	snapshot := csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek:physical-unvalidated", ElementCount: 2, RSSIDBm: []int8{-45}, MeanAmplitude: &mean, PeakAmplitude: &peak}
	store.IngestVendorCSI(snapshot)
	got, ok := store.LatestVendorCSI("mediatek_csi")
	if !ok || got.Source != snapshot.Source || got.RSSIDBm[0] != -45 || got.MeanAmplitude == nil || *got.MeanAmplitude != 9 {
		t.Fatalf("vendor CSI latest snapshot mismatch: %+v, found=%v", got, ok)
	}
	got.RSSIDBm[0] = 0
	gotMean := float32(0)
	got.MeanAmplitude = &gotMean
	again, _ := store.LatestVendorCSI("mediatek_csi")
	if again.RSSIDBm[0] != -45 || *again.MeanAmplitude != 9 {
		t.Fatal("caller mutated stored vendor CSI state")
	}
	if store.NodeCount() != 0 {
		t.Fatal("vendor CSI summary was incorrectly converted into an ESP32 sensing node")
	}
	health := store.Health()
	if health["source"] != snapshot.Source || health["source_state"] != "connected" || health["waiting_for_frames"] != false {
		t.Fatalf("vendor CSI source health is inconsistent: %+v", health)
	}
	store.IngestCSI(protocol.CSIFrame{NodeID: 1, Subcarriers: 1, RSSIDBm: -50, Amplitude: []float64{5}, Phase: []float64{0.1}})
	store.IngestVendorCSI(csivendor.Snapshot{EventType: "qualcomm_csi", Source: "qualcomm", ElementCount: 1})
	if got := store.effectiveSource(true); got != "esp32" {
		t.Fatalf("vendor CSI displaced fresh ESP32 source: %q", got)
	}
}

func TestRadarSnapshotsRemainSeparateAndReturnCopies(t *testing.T) {
	store := New("auto", ws.NewHub())
	peak, power := float32(1.5), float32(7)
	snapshot := radar.Snapshot{EventType: "realtek_radar", Source: "realtek", PeakRangeM: &peak, PeakPower: &power}
	store.IngestRadar(snapshot)
	peak = 99
	got, ok := store.LatestRadar()
	if !ok || got.PeakRangeM == nil || *got.PeakRangeM != 1.5 || got.PeakPower == nil || *got.PeakPower != 7 {
		t.Fatalf("radar snapshot retention mismatch: %+v, found=%v", got, ok)
	}
	*got.PeakRangeM = 88
	again, _ := store.LatestRadar()
	if *again.PeakRangeM != 1.5 {
		t.Fatal("caller mutation changed stored radar snapshot")
	}
	if store.NodeCount() != 0 {
		t.Fatal("radar data was incorrectly converted to a Wi-Fi sensing node")
	}
}

func TestMediaTekDeviceSnapshotsAreSortedFreshAndBounded(t *testing.T) {
	store := New("auto", ws.NewHub())
	store.IngestVendorCSI(csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek", DeviceID: "b", Sequence: 2})
	store.IngestVendorCSI(csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek", DeviceID: "a", Sequence: 1})
	devices := store.MediaTekCSIDevices(time.Now())
	if len(devices) != 2 || devices[0].Snapshot.DeviceID != "a" || devices[1].Snapshot.DeviceID != "b" {
		t.Fatalf("MediaTek devices are not sorted or individually retained: %+v", devices)
	}
	if devices[0].Snapshot.Sequence != 1 || devices[0].AgeMS > 1000 {
		t.Fatalf("MediaTek snapshot metadata or age is incorrect: %+v", devices[0])
	}
	if got := store.MediaTekCSIDevices(time.Now().Add(mediaTekDeviceFreshness + time.Second)); len(got) != 0 {
		t.Fatalf("stale MediaTek snapshots were exposed: %+v", got)
	}
	for i := 0; i < maxMediaTekCSIDevices+1; i++ {
		store.IngestVendorCSI(csivendor.Snapshot{
			EventType: "mediatek_csi", Source: "mediatek", DeviceID: fmt.Sprintf("device-%02d", i), Sequence: uint32(i),
		})
	}
	if got := len(store.MediaTekCSIDevices(time.Now())); got != maxMediaTekCSIDevices {
		t.Fatalf("device registry exceeded its memory bound: got=%d max=%d", got, maxMediaTekCSIDevices)
	}
}

func TestMediaTekPerDeviceFrameRingRetainsBoundedCSIAndReturnsCopies(t *testing.T) {
	store := New("auto", ws.NewHub())
	frame := csivendor.CSIFrame{
		TimestampUS: 100, Sequence: 1, RSSIDBm: []int8{-45}, SubcarrierCount: 2,
		Chains: []csivendor.Chain{{TX: 0, RX: 0, Amplitude: []float32{5, 13}, Phase: []float32{.9, 1.1}}},
	}
	snapshot := csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek", DeviceID: "device-a", Frame: &frame}
	store.IngestVendorCSI(snapshot)
	frame.Chains[0].Amplitude[0] = 99
	store.IngestVendorCSI(csivendor.Snapshot{
		EventType: "mediatek_csi", Source: "mediatek", DeviceID: "device-a",
		Frame: &csivendor.CSIFrame{TimestampUS: 150, Sequence: 2, RSSIDBm: []int8{-46}, SubcarrierCount: 2,
			Chains: []csivendor.Chain{{TX: 0, RX: 0, Amplitude: []float32{6, 14}, Phase: []float32{1, 1.2}}}},
	})
	set, ok := store.MediaTekCSIFrames("device-a", 1)
	if !ok || set.SubcarrierCount != 2 || len(set.Frames) != 1 || set.Frames[0].Sequence != 2 || set.Frames[0].Chains[0].Amplitude[0] != 6 {
		t.Fatalf("latest MediaTek frame mismatch: %+v, found=%v", set, ok)
	}
	set, ok = store.MediaTekCSIFrames("device-a", 10)
	if !ok || len(set.Frames) != 2 || set.Frames[0].Sequence != 1 || set.Frames[0].Chains[0].Amplitude[0] != 5 {
		t.Fatalf("MediaTek frame history was not retained in order: %+v, found=%v", set, ok)
	}
	set.Frames[0].Chains[0].Amplitude[0] = 100
	set, _ = store.MediaTekCSIFrames("device-a", 2)
	if set.Frames[0].Chains[0].Amplitude[0] != 5 {
		t.Fatal("mutating returned MediaTek CSI changed stored ring data")
	}
	if _, ok := store.MediaTekCSIFrames("missing", 10); ok {
		t.Fatal("unknown MediaTek device unexpectedly had a frame ring")
	}
}

func TestWiFiRSSISourceIsDistinctFromESP32CSI(t *testing.T) {
	store := New("wifi", ws.NewHub())
	store.IngestCSI(protocol.CSIFrame{
		Source: "wifi", InputMode: "rssi_derived", NodeID: 0, Subcarriers: 2,
		RSSIDBm: -62, NoiseFloor: -90, Amplitude: []float64{30, 40}, Phase: []float64{0.1, 0.2},
	})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.Source != "wifi" || update.InputMode != "rssi_derived" {
		t.Fatalf("RSSI source provenance was lost: status=%s update=%+v", status, update)
	}
	if update.Nodes[0].PositionAvailable {
		t.Fatal("RSSI observations must not imply a calibrated location")
	}

	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"input_mode":"rssi_derived"`) {
		t.Fatalf("public event omitted measurement provenance: %s", encoded)
	}
}

func TestFreshESP32FramesCannotBeOverwrittenByHostWiFi(t *testing.T) {
	store := New("auto", ws.NewHub())
	store.IngestCSI(protocol.CSIFrame{NodeID: 4, Subcarriers: 2, RSSIDBm: -50, Amplitude: []float64{10, 12}})
	store.IngestCSI(protocol.CSIFrame{Source: "wifi", NodeID: 0, Subcarriers: 1, RSSIDBm: -60, Amplitude: []float64{30}})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.Source != "esp32" || len(update.Nodes) != 1 || update.Nodes[0].NodeID != 4 {
		t.Fatalf("host RSSI overwrote a fresh ESP32 stream: status=%s update=%+v", status, update)
	}
	if health := store.Health(); health["frames_received"].(uint64) != 1 {
		t.Fatalf("ignored Wi-Fi frame was counted as consumed: %+v", health)
	}
}

func TestSyntheticSourceIsNotReportedAsHardwareConnected(t *testing.T) {
	store := New("simulate", ws.NewHub())
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true})
	health := store.Health()
	if health["source"] != "simulate" || health["source_state"] != "synthetic" {
		t.Fatalf("synthetic data was reported as a hardware connection: %+v", health)
	}
}

func TestEdgeCountOnlyPublishesValidatedCount(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: false, Persons: 4, CountValid: false})
	update, _, _ := store.Latest(time.Now())
	if update.EstimatedPersons != nil {
		t.Fatalf("contradictory count was published: %d", *update.EstimatedPersons)
	}
	store.roomDebouncedLevel, store.roomDebounceCandidate = "present_still", "present_still"
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true, Persons: 2, CountValid: true, PresenceScore: 0.8})
	update, _, _ = store.Latest(time.Now())
	if update.EstimatedPersons == nil || *update.EstimatedPersons != 2 {
		t.Fatalf("valid edge count missing: %+v", update.EstimatedPersons)
	}
}

func TestRoomPresenceGatesEdgePersonCount(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.roomDebouncedLevel, store.roomDebounceCandidate = "present_still", "present_still"
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true, Persons: 0, CountValid: true, PresenceScore: 0.8})
	update, _, _ := store.Latest(time.Now())
	if !update.Classification.Presence || update.EstimatedPersons == nil || *update.EstimatedPersons != 1 {
		t.Fatalf("present room did not expose a conservative positive validated count: %+v", update)
	}

	store.roomDebounceCandidate = "absent"
	store.roomDebounceSince = time.Now().Add(-2 * roomDebounceDuration)
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: false, Persons: 0, CountValid: true})
	update, _, _ = store.Latest(time.Now())
	if update.Classification.Presence || update.EstimatedPersons != nil {
		t.Fatalf("absent room retained a positive person count: %+v", update)
	}
}

func TestRoomFusionUsesFreshnessAndSuppressesCountWhenAbsent(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.roomDebouncedLevel, store.roomDebounceCandidate = "present_still", "present_still"
	now := time.Now()
	present := store.getNode(1, now)
	absent := store.getNode(2, now)
	present.mu.Lock()
	present.edgeVitals = &protocol.VitalsPacket{NodeID: 1, Presence: true, Persons: 3, CountValid: true, PresenceScore: 0.9}
	present.vitalsSeen, present.lastSeen = now, now
	present.mu.Unlock()
	absent.mu.Lock()
	absent.edgeVitals = &protocol.VitalsPacket{NodeID: 2}
	absent.vitalsSeen, absent.lastSeen = now.Add(-4*time.Second), now.Add(-4*time.Second)
	absent.mu.Unlock()

	store.publish()
	update, _, _ := store.Latest(time.Now())
	if !update.Classification.Presence || update.Classification.MotionLevel != "present_still" || update.EstimatedPersons == nil || *update.EstimatedPersons != 3 {
		t.Fatalf("fresh presence vote did not win over the older absent vote: %+v", update)
	}

	now = time.Now()
	store.roomDebounceCandidate = "absent"
	store.roomDebounceSince = now.Add(-2 * roomDebounceDuration)
	present.mu.Lock()
	present.vitalsSeen, present.lastSeen = now.Add(-4*time.Second), now.Add(-4*time.Second)
	present.mu.Unlock()
	absent.mu.Lock()
	absent.vitalsSeen, absent.lastSeen = now, now
	absent.mu.Unlock()
	store.publish()
	update, _, _ = store.Latest(time.Now())
	if update.Classification.Presence || update.EstimatedPersons != nil {
		t.Fatalf("fresh absent room vote did not suppress older node count: %+v", update)
	}
}

func TestFeatureStatePreservesFlagsAndWithholdsUncalibratedBiometrics(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.roomDebouncedLevel, store.roomDebounceCandidate = "present_still", "present_still"
	store.IngestFeatureState(protocol.FeatureStatePacket{
		NodeID: 2, MotionScore: 0.3, PresenceScore: 0.9, RespirationBPM: 14,
		RespirationConf: 0.8, HeartbeatBPM: 71, HeartbeatConf: 0.7, NodeCoherence: 0.85,
		QualityFlags: 1 | 2 | 4,
	})
	update, _, _ := store.Latest(time.Now())
	if !update.Classification.Presence {
		t.Fatalf("feature state classification not surfaced: %+v", update)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "breathing_rate_bpm") || strings.Contains(string(encoded), "heartbeat_bpm") || strings.Contains(string(encoded), "heart_rate_bpm") {
		t.Fatalf("uncalibrated biometrics leaked into sensing event: %s", encoded)
	}
	if update.SignalField.GridSize != [3]int{0, 0, 0} {
		t.Fatalf("feature state invented a spatial field: %+v", update.SignalField)
	}
}

func TestFeatureStateMotionAndPresenceAreNotConfused(t *testing.T) {
	store := New("auto", ws.NewHub())
	store.IngestFeatureState(protocol.FeatureStatePacket{NodeID: 2, MotionScore: 0.7, PresenceScore: 0.9, QualityFlags: 1})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.NodeFeatures[0].Classification.MotionLevel != "active" || !update.NodeFeatures[0].Classification.Presence {
		t.Fatalf("feature state classification lost motion/presence: status=%s update=%+v", status, update.NodeFeatures[0].Classification)
	}
	if update.NodeFeatures[0].Classification.MotionLevel != "active" {
		t.Fatalf("node feature classification did not use motion score: %+v", update.NodeFeatures[0].Classification)
	}
}

func TestVitalsClassificationUsesFlagsNotPresenceConfidenceAsMotion(t *testing.T) {
	store := New("auto", ws.NewHub())
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true, PresenceScore: 0.9, CountValid: true})
	update, _, _ := store.Latest(time.Now())
	if update.NodeFeatures[0].Classification.MotionLevel != "present_still" {
		t.Fatalf("high confidence still-presence was misclassified as motion: %s", update.NodeFeatures[0].Classification.MotionLevel)
	}
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Motion: true, PresenceScore: 0.1, CountValid: true})
	update, _, _ = store.Latest(time.Now())
	if update.NodeFeatures[0].Classification.MotionLevel != "present_moving" || !update.NodeFeatures[0].Classification.Presence {
		t.Fatalf("motion flag was not reflected in classification: %+v", update.NodeFeatures[0].Classification)
	}
}

func TestRoomClassificationRequiresContinuousCandidateBeforeChanging(t *testing.T) {
	store := New("esp32", ws.NewHub())
	start := time.Now()
	present := inference.Room{Classification: "present_still", Confidence: 0.8, ContributingNodes: 1}
	if got := store.debounceRoomClassification(present, start); got.Presence || got.MotionLevel != "absent" || got.Confidence != 0.8 {
		t.Fatalf("new room candidate was not held during debounce: %+v", got)
	}
	if got := store.debounceRoomClassification(present, start.Add(roomDebounceDuration-time.Millisecond)); got.Presence {
		t.Fatalf("room changed before debounce window elapsed: %+v", got)
	}
	if got := store.debounceRoomClassification(present, start.Add(roomDebounceDuration)); !got.Presence || got.MotionLevel != "present_still" {
		t.Fatalf("stable room candidate did not activate at the threshold: %+v", got)
	}
	unavailable := inference.Room{Classification: "unavailable"}
	if got := store.debounceRoomClassification(unavailable, start.Add(2*roomDebounceDuration)); !got.Presence {
		t.Fatalf("room cleared before absence debounce window elapsed: %+v", got)
	}
	if got := store.debounceRoomClassification(unavailable, start.Add(3*roomDebounceDuration)); got.Presence || got.MotionLevel != "absent" {
		t.Fatalf("stable absence did not clear room classification: %+v", got)
	}
}

func TestRefreshCompletesDebounceAndPublishesStaleStateWithoutRefreshingDataAge(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true, Persons: 1, CountValid: true, PresenceScore: 0.8})
	first, _, _ := store.Latest(time.Now())
	if first.Classification.Presence {
		t.Fatalf("room presence changed before the debounce window: %+v", first.Classification)
	}
	observedAt := first.observedAt

	store.refreshAt(observedAt.Add(2 * time.Second))
	second := store.latest.Load()
	if second == nil || second.Tick <= first.Tick || !second.Classification.Presence || !second.observedAt.Equal(observedAt) {
		t.Fatalf("refresh did not complete room debounce while preserving input time: first=%+v second=%+v", first, second)
	}

	staleAt := observedAt.Add(latestStale + time.Second)
	store.refreshAt(staleAt)
	third := store.latest.Load()
	_, status, age := store.Latest(staleAt)
	if third == nil || third.Tick <= second.Tick || status != "stale" || age <= latestStale || !third.observedAt.Equal(observedAt) {
		t.Fatalf("stale refresh extended data freshness or failed to publish: status=%s age=%s third=%+v", status, age, third)
	}
	if !third.Classification.Presence {
		t.Fatalf("absence changed before its room debounce completed: %+v", third.Classification)
	}

	store.refreshAt(staleAt.Add(roomDebounceDuration + time.Millisecond))
	fourth := store.latest.Load()
	if fourth == nil || fourth.Classification.Presence || fourth.Classification.MotionLevel != "absent" {
		t.Fatalf("stale room did not transition to absent after debounce: %+v", fourth)
	}
}

func TestAggregatedPresencePreservesQuietCSIIndicator(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.roomDebouncedLevel, store.roomDebounceCandidate = "present_still", "present_still"
	node := store.getNode(1, time.Now())
	node.mu.Lock()
	node.observation = &signal.Observation{
		Timestamp: time.Now(),
		Class: signal.Classification{
			MotionLevel: "present_still", Presence: true, Confidence: 0.5,
		},
	}
	node.lastSeen = node.observation.Timestamp
	node.mu.Unlock()
	store.publish()

	update, _, _ := store.Latest(time.Now())
	if !update.Classification.Presence {
		t.Fatalf("aggregate discarded a valid low-motion CSI presence indicator: %+v", update.Classification)
	}
}

func TestLatestAgesOutFrozenData(t *testing.T) {
	store := New("auto", ws.NewHub())
	store.IngestVitals(protocol.VitalsPacket{NodeID: 1, Presence: true})
	_, status, age := store.Latest(time.Now().Add(latestStale + time.Second))
	if status != "stale" || age <= latestStale {
		t.Fatalf("status=%s age=%s", status, age)
	}
}

func TestMeshSyncSnapshotAndFrameRateState(t *testing.T) {
	store := New("esp32", ws.NewHub())
	base := time.Now()
	node := store.getNode(9, base)
	packet := protocol.SyncPacket{
		NodeID: 9, ProtoVer: protocol.SyncProtocolVersion,
		Flags:   protocol.SyncFlags{IsValid: true, SmoothedUsed: true},
		LocalUS: 28_798_450, EpochUS: 27_634_885, Sequence: 20,
	}
	node.mu.Lock()
	for i := 0; i < 8; i++ {
		frameTime := base.Add(time.Duration(i) * 50 * time.Millisecond)
		node.observeFrameRate(frameTime)
	}
	node.latestSync = &packet
	node.syncSeen = base.Add(400 * time.Millisecond)
	node.mu.Unlock()

	snapshots := store.MeshSync(base.Add(650 * time.Millisecond))
	snapshot, ok := snapshots["9"]
	if !ok || len(snapshots) != 1 {
		t.Fatalf("mesh snapshot missing node: %+v", snapshots)
	}
	if snapshot.OffsetUS != 1_163_565 || !snapshot.IsValid || !snapshot.Smoothed || snapshot.Sequence != 20 {
		t.Fatalf("incorrect sync snapshot: %+v", snapshot)
	}
	if snapshot.CSIFPSSamples < 5 || snapshot.CSIFPSEMA < 19 || snapshot.CSIFPSEMA > 21 {
		t.Fatalf("frame-rate EMA did not converge near 20Hz: %+v", snapshot)
	}
	if snapshot.StalenessMS == nil || *snapshot.StalenessMS != 250 {
		t.Fatalf("unexpected sync staleness: %+v", snapshot.StalenessMS)
	}
	if _, status := store.NodeSync(10, base); status != "unknown_node" {
		t.Fatalf("unknown node status=%q", status)
	}
	if got, status := store.NodeSync(9, base.Add(time.Second)); status != "ok" || got.StalenessMS == nil || *got.StalenessMS != 600 {
		t.Fatalf("node sync status=%q snapshot=%+v", status, got)
	}
}

func TestSyncOnlyNodeIsNotReportedAsSensingNode(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.IngestSync(protocol.SyncPacket{NodeID: 5, ProtoVer: protocol.SyncProtocolVersion})
	if got := store.SnapshotNodes(); len(got) != 0 {
		t.Fatalf("sync-only beacon was reported as a sensing node: %+v", got)
	}
	if _, status := store.NodeSync(5, time.Now()); status != "ok" {
		t.Fatalf("sync-only node lost its mesh record: status=%q", status)
	}
	store.IngestCSI(protocol.CSIFrame{
		NodeID: 5, Antennas: 1, Subcarriers: 1, RSSIDBm: -50, NoiseFloor: -90,
		Amplitude: []float64{12}, Phase: []float64{0.1},
	})
	update, _, _ := store.Latest(time.Now())
	if len(update.Nodes) != 1 || update.Nodes[0].Sync == nil {
		t.Fatalf("sync snapshot was not attached to the sensing node: %+v", update.Nodes)
	}
}

func TestCSIMeshTimestampRecoveryRequiresFreshValidSyncAndFrameFlag(t *testing.T) {
	store := New("esp32", ws.NewHub())
	store.IngestSync(protocol.SyncPacket{
		NodeID: 5, ProtoVer: protocol.SyncProtocolVersion,
		Flags: protocol.SyncFlags{IsValid: true}, LocalUS: 1_100_000, EpochUS: 1_000_000, Sequence: 10,
	})
	store.IngestCSI(protocol.CSIFrame{NodeID: 5, Subcarriers: 1, Sequence: 12, Flags: protocol.CSIFlagMeshSyncValid, RSSIDBm: -50, Amplitude: []float64{10}})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || len(update.Nodes) != 1 || update.Nodes[0].MeshTimestampUS == nil || *update.Nodes[0].MeshTimestampUS != 1_100_000 {
		t.Fatalf("mesh timestamp was not recovered for a valid synchronized frame: status=%s nodes=%+v", status, update.Nodes)
	}

	store.IngestCSI(protocol.CSIFrame{NodeID: 5, Subcarriers: 1, Sequence: 13, RSSIDBm: -50, Amplitude: []float64{10}})
	update, _, _ = store.Latest(time.Now())
	if update.Nodes[0].MeshTimestampUS != nil {
		t.Fatal("frame without sync-valid flag reused prior mesh time")
	}

	store.IngestCSI(protocol.CSIFrame{NodeID: 5, Subcarriers: 1, Sequence: 9, Flags: protocol.CSIFlagMeshSyncValid, RSSIDBm: -50, Amplitude: []float64{10}})
	update, _, _ = store.Latest(time.Now())
	if update.Nodes[0].MeshTimestampUS != nil {
		t.Fatal("frame older than sync sequence was extrapolated")
	}

	node := store.getNode(5, time.Now())
	node.mu.Lock()
	node.syncSeen = time.Now().Add(-meshSyncMaxAge - time.Millisecond)
	node.mu.Unlock()
	store.IngestCSI(protocol.CSIFrame{NodeID: 5, Subcarriers: 1, Sequence: 14, Flags: protocol.CSIFlagMeshSyncValid, RSSIDBm: -50, Amplitude: []float64{10}})
	update, _, _ = store.Latest(time.Now())
	if update.Nodes[0].MeshTimestampUS != nil {
		t.Fatal("stale sync packet was used for timestamp recovery")
	}
}

func TestWASMOutputLatestSnapshotIsCopiedAndWebSocketPublished(t *testing.T) {
	hub := ws.NewHub()
	sub := hub.Subscribe()
	store := New("esp32", hub)
	packet := protocol.WASMOutputPacket{NodeID: 3, ModuleID: 7, Events: []protocol.WASMEvent{{EventType: 2, Value: 1.5}}}
	store.IngestWASMOutput(packet)
	packet.Events[0].Value = 99
	latest, ok := store.LatestWASMOutput()
	if !ok || latest.NodeID != 3 || latest.ModuleID != 7 || len(latest.Events) != 1 || latest.Events[0].Value != 1.5 {
		t.Fatalf("latest WASM event was not isolated from caller mutation: %+v, ok=%v", latest, ok)
	}
	latest.Events[0].Value = 42
	latest, _ = store.LatestWASMOutput()
	if latest.Events[0].Value != 1.5 {
		t.Fatalf("latest WASM event was not isolated from returned mutation: %+v", latest)
	}
	select {
	case frame := <-sub.C:
		var event map[string]any
		if err := json.Unmarshal(frame, &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "wasm_event" || event["node_id"] != float64(3) || event["module_id"] != float64(7) {
			t.Fatalf("unexpected WASM WebSocket frame: %s", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("WASM event was not published")
	}
}

func TestNodeStatusesSeparateCSIStalenessFromOtherPackets(t *testing.T) {
	now := time.Now()
	store := New("esp32", ws.NewHub())
	stale := store.getNode(9, now.Add(-6*time.Second))
	stale.mu.Lock()
	stale.observation = &signal.Observation{
		Timestamp: now.Add(-6 * time.Second),
		Class:     signal.Classification{MotionLevel: "present_moving", Presence: true},
	}
	stale.latestCSISequence, stale.hasCSI = 77, true
	stale.rssiDBm, stale.hasRSSI = -52, true
	stale.edgeVitals = &protocol.VitalsPacket{Persons: 2, CountValid: true}
	stale.mu.Unlock()
	store.IngestSync(protocol.SyncPacket{NodeID: 10, ProtoVer: protocol.SyncProtocolVersion})

	statuses := store.NodeStatuses(now)
	if len(statuses) != 2 || statuses[0].NodeID != 9 || statuses[1].NodeID != 10 {
		t.Fatalf("node status order/count=%+v", statuses)
	}
	if statuses[0].Status != "stale" || statuses[0].LastSeenMS != 6000 || statuses[0].CSIStatus != "stale" || statuses[0].CSILastSeenMS == nil || *statuses[0].CSILastSeenMS != 6000 {
		t.Fatalf("stale CSI status incorrect: %+v", statuses[0])
	}
	if statuses[0].CSISequence == nil || *statuses[0].CSISequence != 77 || statuses[0].RSSIDBm != -52 || statuses[0].PersonCount != 2 || statuses[0].PersonCountValid == nil || !*statuses[0].PersonCountValid {
		t.Fatalf("node telemetry was lost: %+v", statuses[0])
	}
	if statuses[1].LastSeenMS != 999999 || statuses[1].CSILastSeenMS != nil || statuses[1].CSISequence != nil || statuses[1].CSIStatus != "stale" {
		t.Fatalf("sync-only node was presented as a CSI source: %+v", statuses[1])
	}
}
