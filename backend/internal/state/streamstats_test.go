package state

import (
	"testing"
	"time"
)

func TestUpdateRateMeterMeasuresWindowAndDecays(t *testing.T) {
	var meter updateRateMeter
	t0 := time.Now()
	for i := 0; i <= 40; i++ {
		meter.Observe(t0.Add(time.Duration(i) * 50 * time.Millisecond))
	}
	if got := roundOne(meter.RateHz(t0.Add(2 * time.Second))); got != 20 {
		t.Fatalf("50ms cadence should measure 20 updates/s, got %.1f", got)
	}
	if got := meter.RateHz(t0.Add(8 * time.Second)); got != 0 {
		t.Fatalf("rate should decay to zero after the source stalls, got %.1f", got)
	}
}

func TestStreamMetricsUsesFreshSortedNodesAndHonestWarmup(t *testing.T) {
	store := New("auto", nil)
	now := time.Now()
	store.nodesMu.Lock()
	store.nodes[2] = &nodeRecord{hasCSI: true, lastFrameArrival: now, csiFPSEMA: 46, csiFPSSamples: 50}
	store.nodes[1] = &nodeRecord{hasCSI: true, lastFrameArrival: now, csiFPSEMA: 41, csiFPSSamples: 50}
	store.nodes[4] = &nodeRecord{hasCSI: true, lastFrameArrival: now, csiFPSEMA: 20, csiFPSSamples: 2}
	store.nodes[3] = &nodeRecord{hasCSI: true, lastFrameArrival: now.Add(-nodeFreshness - time.Millisecond), csiFPSEMA: 30, csiFPSSamples: 50}
	store.nodesMu.Unlock()

	for i := 0; i <= 40; i++ {
		store.updateRate.Observe(now.Add(time.Duration(i-40) * 50 * time.Millisecond))
	}
	metrics := store.StreamMetrics(now)
	if !metrics.Active || metrics.FPS != 20 || metrics.CSIFPSTotal != 87 {
		t.Fatalf("unexpected aggregate metrics: %+v", metrics)
	}
	if len(metrics.Nodes) != 3 || metrics.Nodes[0].NodeID != 1 || metrics.Nodes[1].NodeID != 2 || metrics.Nodes[2].NodeID != 4 {
		t.Fatalf("fresh node list should be sorted and omit stale node: %+v", metrics.Nodes)
	}
	if metrics.Nodes[0].CSIFPS == nil || *metrics.Nodes[0].CSIFPS != 41 || metrics.Nodes[2].CSIFPS != nil {
		t.Fatalf("node fps must match the Rust warmup contract: %+v", metrics.Nodes)
	}
}

func TestUpdateRateMeterBoundsRetainedSamples(t *testing.T) {
	var meter updateRateMeter
	t0 := time.Now()
	for i := 0; i < updateRateMaxSamples*3; i++ {
		meter.Observe(t0.Add(time.Duration(i) * time.Microsecond))
	}
	if len(meter.times) > updateRateMaxSamples {
		t.Fatalf("rate meter retained %d timestamps", len(meter.times))
	}
}

func TestStaleStatusRefreshDoesNotCountAsSensorTraffic(t *testing.T) {
	store := New("auto", nil)
	lastInput := time.Now()
	store.lastInputAt.Store(lastInput.UnixNano())

	refreshAt := lastInput.Add(nodeFreshness + time.Second)
	store.refreshAt(refreshAt)
	metrics := store.StreamMetrics(refreshAt.Add(time.Second))
	if metrics.Active || metrics.FPS != 0 {
		t.Fatalf("stale status refresh must not keep stream active: %+v", metrics)
	}
	if retained := len(store.updateRate.times); retained != 0 {
		t.Fatalf("stale status refresh counted %d sensor updates", retained)
	}
}
