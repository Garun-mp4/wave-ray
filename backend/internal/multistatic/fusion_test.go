package multistatic

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestFuseLatestUsesMeshEpochCanonicalGridAndIdentityKeyedPositions(t *testing.T) {
	now := time.Unix(100, 0)
	firstMesh, secondMesh := uint64(1_000_000), uint64(1_012_000)
	first := testFrame(9, 64, 2, now)
	first.MeshTimestampUS = &firstMesh
	first.PhaseReference = PhaseNodeLocal
	first.Phase = make([]float64, len(first.Amplitude))
	first.Position = [3]float64{4, 5, 6}
	first.PositionAvailable = true
	second := testFrame(2, 128, 4, now)
	second.MeshTimestampUS = &secondMesh
	second.PhaseReference = PhaseNodeLocal
	second.Phase = make([]float64, len(second.Amplitude))
	second.Position = [3]float64{1, 2, 3}
	second.PositionAvailable = true

	got := FuseLatest([]Frame{first, second}, now, DefaultOptions())
	if got.Status != "fused" || !got.Fused || !got.Available || got.ActiveNodes != 2 {
		t.Fatalf("fusion status=%+v", got)
	}
	if got.TimeAlignment != "mesh_epoch" || !got.Synchronized || got.TimestampSpreadUS != 12_000 {
		t.Fatalf("mesh alignment metadata=%+v", got)
	}
	if len(got.FusedAmplitude) != CanonicalSubcarriers || math.Abs(got.FusedAmplitude[0]-3) > 1e-9 || math.Abs(got.FusedAmplitude[len(got.FusedAmplitude)-1]-3) > 1e-9 {
		t.Fatalf("canonical fused amplitude length=%d first=%f last=%f", len(got.FusedAmplitude), got.FusedAmplitude[0], got.FusedAmplitude[len(got.FusedAmplitude)-1])
	}
	if got.TimestampUS == nil || *got.TimestampUS != secondMesh || got.CrossNodeCoherence < 0.999999 {
		t.Fatalf("timestamp or attention coherence=%+v", got)
	}
	if got.PhaseFusion.Mode != "amplitude_only" || got.PhaseFusion.Reason != "independent_clocks" || len(got.FusedPhase) != 0 {
		t.Fatalf("independent ESP clocks were combined: phase=%+v values=%d", got.PhaseFusion, len(got.FusedPhase))
	}
	if !got.GeometryConfigured || got.ConfiguredPositionNodes != 2 || !reflect.DeepEqual(got.ContributorNodeIDs, []int{2, 9}) {
		t.Fatalf("contributor identities/geometry=%+v", got)
	}
	if got.Contributors[0].NodeID != 2 || got.Contributors[0].Position != [3]float64{1, 2, 3} || got.Contributors[1].NodeID != 9 || got.Contributors[1].Position != [3]float64{4, 5, 6} {
		t.Fatalf("positions were not looked up by node ID: %+v", got.Contributors)
	}
}

func TestFuseLatestFallsBackToOneHostArrivalDomain(t *testing.T) {
	now := time.Unix(200, 0)
	a, b := testFrame(1, 56, 1, now), testFrame(7, 56, 3, now)
	mesh := uint64(50_000)
	a.MeshTimestampUS = &mesh
	a.ArrivalTimestampUS = 1_000_000
	b.MeshTimestampUS = nil
	b.ArrivalTimestampUS = 1_010_000

	got := FuseLatest([]Frame{a, b}, now, DefaultOptions())
	if got.Status != "fused" || got.TimeAlignment != "host_arrival_estimate" || got.Synchronized {
		t.Fatalf("fallback mixed or misreported timestamp domains: %+v", got)
	}
	if got.TimestampSpreadUS != 10_000 || got.ActiveNodes != 2 {
		t.Fatalf("arrival cohort spread=%d nodes=%d", got.TimestampSpreadUS, got.ActiveNodes)
	}
}

func TestFuseLatestRejectsOutOfWindowAndStaleContributors(t *testing.T) {
	now := time.Unix(300, 0)
	old := testFrame(3, 56, 5, now)
	old.MeshTimestampUS = u64(100)
	old.ArrivalTimestampUS = 100_000
	newest := testFrame(8, 56, 10, now)
	newest.MeshTimestampUS = u64(100_000)
	newest.ArrivalTimestampUS = 200_000
	stale := testFrame(10, 56, 20, now.Add(-6*time.Second))
	stale.ArrivalTimestampUS = 200_001

	got := FuseLatest([]Frame{old, newest, stale}, now, DefaultOptions())
	if got.Status != "single_node" || got.Fused || got.ActiveNodes != 1 || got.CandidateNodes != 2 {
		t.Fatalf("unexpected fallback after excluding incoherent/stale frames: %+v", got)
	}
	if !reflect.DeepEqual(got.OutOfWindowNodeIDs, []int{3}) {
		t.Fatalf("out-of-window nodes=%v", got.OutOfWindowNodeIDs)
	}
	if got.TimeAlignment != "host_arrival_estimate" || got.Synchronized || got.AlignmentQuality != "tight" {
		t.Fatalf("unverified host times were labeled synchronized: %+v", got)
	}
}

func TestSinglePositionedNodeDoesNotClaimSpatialGeometry(t *testing.T) {
	now := time.Unix(350, 0)
	frame := testFrame(5, 56, 3, now)
	frame.ArrivalTimestampUS = 25_000
	frame.Position = [3]float64{1, 2, 3}
	frame.PositionAvailable = true

	got := FuseLatest([]Frame{frame}, now, DefaultOptions())
	if got.Status != "single_node" || got.Fused || !got.Available {
		t.Fatalf("single sensor status=%+v", got)
	}
	if got.GeometryConfigured || got.ConfiguredPositionNodes != 1 || !got.Contributors[0].PositionAvailable {
		t.Fatalf("single configured position was misreported as multistatic geometry: %+v", got)
	}
	if got.AlignmentQuality != "tight" {
		t.Fatalf("single-frame alignment quality should reflect its zero timestamp spread: %+v", got)
	}
}

func TestFuseLatestFailsClosedOnRSSIAndMalformedCSI(t *testing.T) {
	now := time.Unix(400, 0)
	wifi := testFrame(1, 56, 1, now)
	wifi.Source, wifi.InputMode = "wifi", "rssi_derived"
	malformed := testFrame(2, 56, 2, now)
	malformed.Antennas = 2
	got := FuseLatest([]Frame{wifi, malformed}, now, DefaultOptions())
	if got.Available || got.Fused || got.Status != "unavailable" {
		t.Fatalf("non-CSI or malformed samples were fused: %+v", got)
	}
	if !reflect.DeepEqual(got.RejectedNodeIDs, []int{1, 2}) {
		t.Fatalf("rejected node identities=%v", got.RejectedNodeIDs)
	}
}

func TestSharedPhaseReferenceIsRequiredForCoherentFusion(t *testing.T) {
	now := time.Unix(500, 0)
	a, b := testFrame(1, 56, 1, now), testFrame(2, 56, 1, now)
	a.MeshTimestampUS, b.MeshTimestampUS = u64(1_000), u64(1_001)
	a.PhaseReference, b.PhaseReference = PhaseShared, PhaseShared
	a.PhaseReferenceID, b.PhaseReferenceID = 42, 42
	a.Phase, b.Phase = make([]float64, 56), make([]float64, 56)
	for i := range a.Phase {
		a.Phase[i], b.Phase[i] = 0.1, 0.3
	}

	got := FuseLatest([]Frame{a, b}, now, DefaultOptions())
	if got.PhaseFusion.Mode != "coherent" || got.PhaseFusion.ReferenceID == nil || *got.PhaseFusion.ReferenceID != 42 {
		t.Fatalf("valid shared phase reference was not recorded: %+v", got.PhaseFusion)
	}
	if len(got.FusedPhase) != CanonicalSubcarriers || math.Abs(got.FusedPhase[0]-0.2) > 1e-6 {
		t.Fatalf("circular phase fusion=%v", got.FusedPhase[:min(len(got.FusedPhase), 2)])
	}
	b.PhaseReferenceID = 43
	got = FuseLatest([]Frame{a, b}, now, DefaultOptions())
	if got.PhaseFusion.Mode != "amplitude_only" || got.PhaseFusion.Reason != "mixed_references" || len(got.FusedPhase) != 0 {
		t.Fatalf("mixed references were combined: %+v", got.PhaseFusion)
	}
}

func TestFuseLatestKeepsSyntheticEvidenceLabel(t *testing.T) {
	now := time.Unix(600, 0)
	a, b := testFrame(1, 56, 1, now), testFrame(2, 56, 1, now)
	a.Source, b.Source = "simulate", "simulate"
	a.ArrivalTimestampUS, b.ArrivalTimestampUS = 1_000, 1_001
	got := FuseLatest([]Frame{a, b}, now, DefaultOptions())
	if got.Evidence != "synthetic" || got.TimeAlignment != "host_arrival_estimate" {
		t.Fatalf("synthetic provenance was lost: %+v", got)
	}
}

func testFrame(nodeID uint8, subcarriers int, amplitude float64, seenAt time.Time) Frame {
	values := make([]float64, subcarriers)
	for i := range values {
		values[i] = amplitude
	}
	return Frame{
		NodeID: nodeID, Antennas: 1, Subcarriers: subcarriers,
		Amplitude: values, Source: "esp32", SeenAt: seenAt,
	}
}

func u64(value uint64) *uint64 { return &value }
