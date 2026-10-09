package calibration

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

func newTestManager(t *testing.T) (*Manager, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	manager, err := New(filepath.Join(t.TempDir(), "room_calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now }
	return manager, now
}

func testFrame(node uint8, sequence uint32, subcarriers uint16) protocol.CSIFrame {
	amplitude := make([]float64, int(subcarriers))
	for i := range amplitude {
		amplitude[i] = 1 + float64(i)/100
	}
	return protocol.CSIFrame{NodeID: node, Sequence: sequence, Subcarriers: subcarriers, PPDUType: 0, Amplitude: amplitude}
}

func seedGridEvidence(manager *Manager, now time.Time, nodeID uint8, subcarriers uint16) {
	for i := 0; i <= 30; i++ {
		at := now.Add(-12*time.Second + time.Duration(i)*400*time.Millisecond)
		manager.Observe(testFrame(nodeID, uint32(i), subcarriers), at)
	}
}

func startSingleNode(t *testing.T, manager *Manager, now time.Time, nodeID uint8) Identity {
	t.Helper()
	seedGridEvidence(manager, now, nodeID, 56)
	result, err := manager.Start(StartRequest{BindingDigest: strings.Repeat("a", 64), SourceNodeIDs: []int{int(nodeID)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := result["session_id"].(string)
	digest := result["binding_digest"].(string)
	return Identity{BootEpoch: result["boot_epoch"].(string), SessionID: &sessionID, BindingDigest: &digest, SourceNodeIDs: []int{int(nodeID)}}
}

func TestMatchesIdentityDoesNotExposeOrConfuseCalibrationSessions(t *testing.T) {
	manager, now := newTestManager(t)
	identity := startSingleNode(t, manager, now, 7)
	if !manager.MatchesIdentity(identity) {
		t.Fatal("active calibration identity was not recognized")
	}

	stale := identity
	wrongDigest := strings.Repeat("b", 64)
	stale.BindingDigest = &wrongDigest
	if manager.MatchesIdentity(stale) {
		t.Fatal("identity with a mismatched room binding was accepted")
	}
	if (*Manager)(nil).MatchesIdentity(identity) {
		t.Fatal("nil calibration manager reported an active identity")
	}
}

func TestGridCandidatesRequireStableRawCSI(t *testing.T) {
	manager, now := newTestManager(t)
	for i := 0; i < 29; i++ {
		manager.Observe(testFrame(4, uint32(i), 64), now.Add(-9*time.Second+time.Duration(i)*300*time.Millisecond))
	}
	candidates := manager.Candidates(4, now)
	if len(candidates) != 1 || candidates[0].Eligible {
		t.Fatalf("grid without 10s span became eligible: %+v", candidates)
	}
	manager.Observe(testFrame(4, 30, 64), now.Add(-11*time.Second)) // out-of-order arrival timestamp
	manager.Observe(testFrame(4, 31, 64), now)
	candidates = manager.Candidates(4, now)
	if len(candidates) != 1 || !candidates[0].Eligible || candidates[0].Grid.NSubcarriers != 64 || candidates[0].Evidence.RateHz < gridMinimumRateHz || candidates[0].Evidence.MaxGapS >= gridMaximumGap.Seconds() {
		t.Fatalf("stable grid was not selected with expected evidence: %+v", candidates)
	}
	manager.Observe(testFrame(4, 32, 56), now)
	if got := manager.Candidates(4, now); len(got) != 2 {
		t.Fatalf("expected candidates grouped by grid identity, got %+v", got)
	}
	badPPDU := testFrame(5, 1, 64)
	badPPDU.PPDUType = 4
	for i := 0; i <= 30; i++ {
		manager.Observe(badPPDU, now.Add(-12*time.Second+time.Duration(i)*400*time.Millisecond))
	}
	for _, candidate := range manager.Candidates(5, now) {
		if candidate.Eligible {
			t.Fatalf("unsupported PPDU grid became eligible: %+v", candidate)
		}
	}
}

func TestStartValidatesBindingAndSelectsEligibleSourceGrid(t *testing.T) {
	manager, now := newTestManager(t)
	seedGridEvidence(manager, now, 8, 64)
	request := StartRequest{BindingDigest: strings.Repeat("A", 64), SourceNodeIDs: []int{8}}
	if _, err := manager.Start(request, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("uppercase digest error=%v", err)
	}
	request.BindingDigest = strings.Repeat("a", 64)
	request.SourceNodeIDs = []int{8, 7}
	if _, err := manager.Start(request, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("unsorted source IDs error=%v", err)
	}
	request.SourceNodeIDs = []int{7, 8}
	result, err := manager.Start(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["source_node_id"] != uint8(8) || result["source_grid"].(Grid).NSubcarriers != 64 {
		t.Fatalf("wrong selected source/grid: %+v", result)
	}
	if _, err := manager.Start(request, nil); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("parallel start error=%v", err)
	}
}

func TestSequenceAdmissionHandlesWrapDuplicatesReorderAndFault(t *testing.T) {
	manager, now := newTestManager(t)
	identity := startSingleNode(t, manager, now, 5)
	manager.Observe(testFrame(5, math.MaxUint32, 56), now.Add(time.Second))
	manager.Observe(testFrame(5, 0, 56), now.Add(2*time.Second))
	manager.Observe(testFrame(5, 0, 56), now.Add(3*time.Second))
	manager.Observe(testFrame(5, math.MaxUint32, 56), now.Add(4*time.Second))
	manager.mu.RLock()
	session := manager.session
	if session.FrameCount != 2 || session.LastSequence[5] != 0 || session.Reordered[5] != 1 || session.MaxReorderDepth[5] != 1 {
		manager.mu.RUnlock()
		t.Fatalf("wrap/duplicate/reorder admission is wrong: %+v", session)
	}
	manager.mu.RUnlock()
	manager.Observe(testFrame(5, 0xffff_ff00, 56), now.Add(5*time.Second))
	if _, err := manager.Stop(identity, now.Add(6*time.Second)); !errors.Is(err, ErrSequenceDiscontinuity) {
		t.Fatalf("large backward sequence jump did not block finalization: %v", err)
	}
}

func TestStopPersistsAggregateModelAndResetRemovesIt(t *testing.T) {
	manager, now := newTestManager(t)
	identity := startSingleNode(t, manager, now, 5)
	manager.Observe(testFrame(5, 100, 56), now.Add(time.Second))
	manager.mu.Lock()
	manager.session.FrameCount = minimumFrames
	manager.session.FirstFrameAt = now.Add(-minimumDuration)
	manager.session.LastSeen[5] = now
	for i := range manager.session.Mean {
		manager.session.Mean[i] = float64(i) / 10
		manager.session.M2[i] = float64(i)
	}
	path := manager.path
	manager.mu.Unlock()
	result, err := manager.Stop(identity, now)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "calibrated" || result["numeric_vitals_authorized"] != false || result["model_kind"] != "mean_variance_reference_only" {
		t.Fatalf("unexpected finalize response: %+v", result)
	}
	if _, err := manager.Stop(identity, now.Add(modelLifetime+time.Second)); !errors.Is(err, ErrCalibrationExpired) {
		t.Fatalf("expired model was returned as fresh: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "amplitude") || strings.Contains(string(data), "raw_csi") || !strings.Contains(string(data), "variance") || !strings.Contains(string(data), `"source_node_ids":[5]`) {
		t.Fatalf("persisted calibration is not an aggregate-only model: %s", data)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	status := reloaded.Status(now.Add(time.Second))
	if status["status"] != "stored" || status["calibrated"] != true || status["model_id"] == nil {
		t.Fatalf("model was not loaded after restart: %+v", status)
	}
	trust := reloaded.Trust(nil, nil, nil, now.Add(time.Second))
	if trust["authorized"] != false {
		t.Fatalf("stored baseline must not authorize measurements: %+v", trust)
	}
	if _, err := reloaded.SetDedupFactor(7); err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Reset(Identity{BootEpoch: reloaded.bootEpoch}); err != nil {
		t.Fatal(err)
	}
	afterReset, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterReset.Status(now)["model_id"] != nil || afterReset.DedupFactor() != 7 {
		t.Fatalf("reset should delete model while preserving dedup setting: %+v factor=%v", afterReset.Status(now), afterReset.DedupFactor())
	}
}

func TestStopRejectsMissingBoundContributorAndInsufficientCapture(t *testing.T) {
	manager, now := newTestManager(t)
	seedGridEvidence(manager, now, 5, 56)
	result, err := manager.Start(StartRequest{BindingDigest: strings.Repeat("b", 64), SourceNodeIDs: []int{5, 6}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, digest := result["session_id"].(string), result["binding_digest"].(string)
	identity := Identity{BootEpoch: result["boot_epoch"].(string), SessionID: &sessionID, BindingDigest: &digest, SourceNodeIDs: []int{5, 6}}
	manager.Observe(testFrame(5, 1_000, 56), now)
	if _, err := manager.Stop(identity, now); !errors.Is(err, ErrMissingContributors) {
		t.Fatalf("missing device contributor error=%v", err)
	}
	manager.Observe(testFrame(6, 1, 56), now)
	if _, err := manager.Stop(identity, now); !errors.Is(err, ErrInsufficientEvidence) {
		t.Fatalf("short capture was finalized: %v", err)
	}
}

func TestGroundTruthFactorBoundsAndValidatesCount(t *testing.T) {
	if _, err := GroundTruthFactor(10, 0); !errors.Is(err, ErrInvalidGroundTruth) {
		t.Fatalf("zero count error=%v", err)
	}
	if got, _ := GroundTruthFactor(20, 1); got != 10 {
		t.Fatalf("factor upper bound=%v", got)
	}
	if got, _ := GroundTruthFactor(1, 2); got != 1 {
		t.Fatalf("factor lower bound=%v", got)
	}
	if got, _ := GroundTruthFactor(0, 2); got != defaultDedupFactor {
		t.Fatalf("no fresh telemetry should preserve safe default=%v", got)
	}
}

func TestStoredCalibrationRejectsCorruptOrUnexpectedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "room_calibration.json")
	if err := os.WriteFile(path, []byte(`{"schema":"old","dedup_factor":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("corrupt or unsupported state file was silently accepted")
	}
	if err := os.WriteFile(path, []byte(`{"schema":"ruview.calibration.state.v1","dedup_factor":3,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("unknown persisted state field was silently accepted")
	}
}
