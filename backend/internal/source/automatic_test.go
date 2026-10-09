package source

import (
	"context"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/state"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func TestAutomaticSourcePromotesAndStopsSimulationOnLiveFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := state.New("auto", ws.NewHub())
	automatic, err := StartAutomatic(ctx, store, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer automatic.Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		health := store.Health()
		if health["source"] == "simulate" && health["frames_received"].(uint64) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if health := store.Health(); health["source"] != "simulate" || health["frames_received"].(uint64) == 0 {
		t.Fatalf("automatic mode did not expose its synthetic fallback: %+v", health)
	}

	automatic.IngestCSI(protocol.CSIFrame{
		NodeID: 9, Antennas: 1, Subcarriers: 2, RSSIDBm: -48, NoiseFloor: -90,
		Amplitude: []float64{12, 13}, Phase: []float64{0.1, 0.2},
	})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.Source != "esp32" || len(update.Nodes) != 1 || update.Nodes[0].NodeID != 9 {
		t.Fatalf("live frame did not replace synthetic state: status=%s update=%+v", status, update)
	}
	tick := update.Tick
	time.Sleep(20 * time.Millisecond)
	update, _, _ = store.Latest(time.Now())
	if update.Tick != tick {
		t.Fatalf("synthetic stream continued after live source promotion: tick %d -> %d", tick, update.Tick)
	}
}

func TestStartAutomaticRequiresDependencies(t *testing.T) {
	if _, err := StartAutomatic(nil, state.New("auto", nil), time.Second); err == nil {
		t.Fatal("nil parent context was accepted")
	}
	if _, err := StartAutomatic(context.Background(), nil, time.Second); err == nil {
		t.Fatal("nil state store was accepted")
	}
}

func TestSyncBeaconDoesNotPromoteSyntheticSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := state.New("auto", ws.NewHub())
	automatic, err := StartAutomatic(ctx, store, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer automatic.Stop()

	automatic.IngestSync(protocol.SyncPacket{
		NodeID: 3, ProtoVer: protocol.SyncProtocolVersion,
		Flags:   protocol.SyncFlags{IsLeader: true, IsValid: true},
		LocalUS: 1_000_000, EpochUS: 1_000_000,
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.Health()["frames_received"].(uint64) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	health := store.Health()
	if health["source"] != "simulate" || health["source_state"] != "synthetic" || health["frames_received"].(uint64) == 0 {
		t.Fatalf("a sync beacon incorrectly promoted the source: %+v", health)
	}
	if health["sync_packets"].(uint64) != 1 {
		t.Fatalf("sync packet counter=%v, want 1", health["sync_packets"])
	}
}

func TestWiFiPromotesFallbackAndLaterESP32TakesPriority(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := state.New("auto", ws.NewHub())
	automatic, err := StartAutomatic(ctx, store, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer automatic.Stop()

	automatic.IngestWiFi(protocol.CSIFrame{
		NodeID: 0, Subcarriers: 2, RSSIDBm: -60, Amplitude: []float64{20, 22},
	})
	update, status, _ := store.Latest(time.Now())
	if status != "fresh" || update.Source != "wifi" || update.InputMode != "rssi_derived" {
		t.Fatalf("Wi-Fi did not promote the simulation with clear provenance: %s %+v", status, update)
	}
	tick := update.Tick
	time.Sleep(20 * time.Millisecond)
	update, _, _ = store.Latest(time.Now())
	if update.Tick != tick {
		t.Fatalf("synthetic ticks continued after Wi-Fi promotion: %d -> %d", tick, update.Tick)
	}

	automatic.IngestCSI(protocol.CSIFrame{
		NodeID: 8, Subcarriers: 2, RSSIDBm: -49, Amplitude: []float64{9, 11},
	})
	update, _, _ = store.Latest(time.Now())
	if update.Source != "esp32" || len(update.Nodes) != 1 || update.Nodes[0].NodeID != 8 {
		nodeIDs := make([]uint8, 0, len(update.Nodes))
		for _, node := range update.Nodes {
			nodeIDs = append(nodeIDs, node.NodeID)
		}
		t.Fatalf("a later ESP32 frame did not replace Wi-Fi state: source=%s node_ids=%v", update.Source, nodeIDs)
	}
}

func TestVendorCSIPromotesAndStopsSyntheticFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := state.New("auto", ws.NewHub())
	automatic, err := StartAutomatic(ctx, store, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer automatic.Stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && store.Health()["frames_received"].(uint64) == 0 {
		time.Sleep(time.Millisecond)
	}
	mean, peak := float32(12), float32(16)
	automatic.IngestVendorCSI(csivendor.Snapshot{EventType: "mediatek_csi", Source: "mediatek:physical-unvalidated", ElementCount: 1, MeanAmplitude: &mean, PeakAmplitude: &peak})
	health := store.Health()
	if health["source"] != "mediatek:physical-unvalidated" || health["source_state"] != "connected" || health["waiting_for_frames"] != false {
		t.Fatalf("vendor CSI did not replace fallback with live source provenance: %+v", health)
	}
	tick := health["tick"].(uint64)
	time.Sleep(20 * time.Millisecond)
	if got := store.Health()["tick"].(uint64); got != tick {
		t.Fatalf("synthetic stream continued after vendor CSI promotion: %d -> %d", tick, got)
	}
}
