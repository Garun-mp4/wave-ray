// Package calibration manages room-bound CSI baseline captures. It persists
// aggregate statistics only; raw CSI is never written to disk.
package calibration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

const (
	modelSchema                  = "ruview.calibration.baseline-model.v1"
	receiptSchema                = "ruview.calibration.baseline-receipt.v1"
	persistSchema                = "ruview.calibration.state.v1"
	canonicalSubcarriers         = 56
	minimumFrames         uint64 = 1_000
	minimumDuration              = 10 * time.Minute
	modelLifetime                = 24 * time.Hour
	gridEvidenceWindow           = 20 * time.Second
	gridEvidenceCapacity         = 1_024
	gridMinimumSpan              = 10 * time.Second
	gridMinimumRateHz            = 2.0
	gridMaximumGap               = 5 * time.Second
	maximumReorderDepth   uint32 = 16
	maximumTrackedNodes          = 64
	maximumStateFileBytes        = 1 << 20
	// DefaultDedupFactor preserves the Rust server's default for fresh node-count aggregation.
	DefaultDedupFactor = 3.0
	defaultDedupFactor = DefaultDedupFactor
)

var (
	ErrInvalidBinding        = errors.New("invalid calibration binding")
	ErrSourceUnavailable     = errors.New("calibration source unavailable")
	ErrGridUnavailable       = errors.New("eligible CSI grid is unavailable")
	ErrAlreadyActive         = errors.New("calibration is already active")
	ErrFreshModel            = errors.New("a fresh calibration model already exists")
	ErrCalibrationExpired    = errors.New("calibration model is no longer fresh")
	ErrIdentityMismatch      = errors.New("calibration identity does not match the active session")
	ErrNotCollecting         = errors.New("calibration is not collecting")
	ErrSequenceDiscontinuity = errors.New("calibration CSI sequence discontinuity")
	ErrMissingContributors   = errors.New("one or more bound devices have not contributed CSI")
	ErrGridStale             = errors.New("the bound CSI grid is stale")
	ErrInsufficientEvidence  = errors.New("calibration evidence is insufficient")
	ErrInvalidGroundTruth    = errors.New("ground-truth count must be a positive integer")
)

// Grid identifies a CSI tone layout. PPDU type is part of the identity because
// equal subcarrier counts can represent different PHY tone maps.
type Grid struct {
	NSubcarriers uint16 `json:"n_subcarriers"`
	PPDUType     uint8  `json:"ppdu_type"`
}

type GridEvidence struct {
	SampleCount int     `json:"sample_count"`
	SpanS       float64 `json:"span_s"`
	RateHz      float64 `json:"rate_hz"`
	MaxGapS     float64 `json:"max_gap_s"`
	LatestAgeS  float64 `json:"latest_age_s"`
}

type GridCandidate struct {
	Grid     Grid         `json:"grid"`
	Evidence GridEvidence `json:"evidence"`
	Eligible bool         `json:"eligible"`
}

type StartRequest struct {
	BindingDigest string `json:"binding_digest"`
	SourceNodeIDs []int  `json:"source_node_ids"`
}

type Identity struct {
	BootEpoch     string  `json:"boot_epoch"`
	SessionID     *string `json:"session_id,omitempty"`
	BindingDigest *string `json:"binding_digest,omitempty"`
	SourceNodeIDs []int   `json:"source_node_ids,omitempty"`
}

type Receipt struct {
	Schema        string    `json:"schema"`
	BootEpoch     string    `json:"boot_epoch"`
	SessionID     string    `json:"session_id"`
	ModelID       string    `json:"model_id"`
	BindingDigest string    `json:"binding_digest"`
	SourceNodeIDs []int     `json:"source_node_ids"`
	SourceNodeID  uint8     `json:"source_node_id"`
	SourceGrid    Grid      `json:"source_grid"`
	FrameCount    uint64    `json:"frame_count"`
	CompletedAt   time.Time `json:"completed_at"`
}

// Model stores the room's per-tone mean and variance. These are privacy-
// reduced aggregate references, not raw CSI or a person-localization model.
type Model struct {
	Schema        string    `json:"schema"`
	ID            string    `json:"model_id"`
	BindingDigest string    `json:"binding_digest"`
	SourceNodeIDs []int     `json:"source_node_ids"`
	SourceNodeID  uint8     `json:"source_node_id"`
	SourceGrid    Grid      `json:"source_grid"`
	FrameCount    uint64    `json:"frame_count"`
	CalibratedAt  time.Time `json:"calibrated_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Mean          []float64 `json:"mean"`
	Variance      []float64 `json:"variance"`
	Receipt       Receipt   `json:"receipt"`
}

type session struct {
	Identity        Identity
	ModelID         string
	SourceNodeID    uint8
	SourceGrid      Grid
	GridEvidence    GridEvidence
	Status          string
	StartedAt       time.Time
	FirstFrameAt    time.Time
	LastSequence    map[uint8]uint32
	ObservedNodes   map[uint8]struct{}
	LastSeen        map[uint8]time.Time
	Reordered       map[uint8]uint64
	MaxReorderDepth map[uint8]uint32
	SequenceFaults  map[uint8]struct{}
	FrameCount      uint64
	Mean            [canonicalSubcarriers]float64
	M2              [canonicalSubcarriers]float64
}

type gridSample struct {
	grid Grid
	at   time.Time
}

type persistedState struct {
	Schema      string  `json:"schema"`
	DedupFactor float64 `json:"dedup_factor"`
	Model       *Model  `json:"model,omitempty"`
}

// Manager serializes session transitions, model persistence, and the tuning
// value so concurrent CSI ingest and HTTP control cannot race the calibration.
type Manager struct {
	mu          sync.RWMutex
	path        string
	bootEpoch   string
	now         func() time.Time
	evidence    map[uint8][]gridSample
	session     *session
	model       *Model
	dedupFactor float64
}

func New(path string) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("calibration state path is empty")
	}
	boot, err := randomID("boot")
	if err != nil {
		return nil, fmt.Errorf("create calibration boot identity: %w", err)
	}
	m := &Manager{
		path: path, bootEpoch: boot, now: time.Now,
		evidence: make(map[uint8][]gridSample), dedupFactor: defaultDedupFactor,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Observe(frame protocol.CSIFrame, now time.Time) {
	if m == nil || (frame.Source != "" && frame.Source != "esp32") || frame.InputMode == "rssi_derived" || frame.Subcarriers == 0 {
		return
	}
	grid := Grid{NSubcarriers: frame.Subcarriers, PPDUType: frame.PPDUType}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observeGridLocked(frame.NodeID, grid, now)
	s := m.session
	if s == nil || s.Status == "cancelled" || !containsNode(s.Identity.SourceNodeIDs, frame.NodeID) || grid != s.SourceGrid {
		return
	}
	if _, faulted := s.SequenceFaults[frame.NodeID]; faulted {
		return
	}
	if previous, exists := s.LastSequence[frame.NodeID]; exists {
		delta := frame.Sequence - previous
		switch {
		case delta == 0:
			return
		case delta >= 1<<31:
			depth := previous - frame.Sequence
			if depth <= maximumReorderDepth {
				s.Reordered[frame.NodeID]++
				if depth > s.MaxReorderDepth[frame.NodeID] {
					s.MaxReorderDepth[frame.NodeID] = depth
				}
				return
			}
			s.SequenceFaults[frame.NodeID] = struct{}{}
			return
		}
	}
	if frame.NodeID == s.SourceNodeID && !validAmplitude(frame, int(s.SourceGrid.NSubcarriers)) {
		return
	}
	s.LastSequence[frame.NodeID] = frame.Sequence
	s.ObservedNodes[frame.NodeID] = struct{}{}
	s.LastSeen[frame.NodeID] = now
	if frame.NodeID != s.SourceNodeID || s.Status == "calibrated" {
		return
	}
	values, ok := normalize(frame.Amplitude[:int(frame.Subcarriers)])
	if !ok {
		return
	}
	if s.FirstFrameAt.IsZero() {
		s.FirstFrameAt = now
		s.Status = "collecting"
	}
	s.FrameCount++
	n := float64(s.FrameCount)
	for i, value := range values {
		delta := value - s.Mean[i]
		s.Mean[i] += delta / n
		s.M2[i] += delta * (value - s.Mean[i])
	}
}

func validAmplitude(frame protocol.CSIFrame, width int) bool {
	if width <= 0 || len(frame.Amplitude) < width {
		return false
	}
	for _, value := range frame.Amplitude[:width] {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	return true
}

func normalize(values []float64) ([]float64, bool) {
	if len(values) == 0 {
		return nil, false
	}
	if len(values) == canonicalSubcarriers {
		return append([]float64(nil), values...), true
	}
	result := make([]float64, canonicalSubcarriers)
	for i := range result {
		position := float64(i) * float64(len(values)-1) / float64(canonicalSubcarriers-1)
		left := int(position)
		right := min(left+1, len(values)-1)
		fraction := position - float64(left)
		result[i] = values[left]*(1-fraction) + values[right]*fraction
	}
	return result, true
}

func (m *Manager) observeGridLocked(nodeID uint8, grid Grid, now time.Time) {
	if _, exists := m.evidence[nodeID]; !exists && len(m.evidence) >= maximumTrackedNodes {
		var oldestID uint8
		var oldest time.Time
		for id, samples := range m.evidence {
			seen := time.Time{}
			if len(samples) > 0 {
				seen = samples[len(samples)-1].at
			}
			if oldest.IsZero() || seen.Before(oldest) {
				oldestID, oldest = id, seen
			}
		}
		delete(m.evidence, oldestID)
	}
	samples := m.evidence[nodeID]
	cutoff := now.Add(-gridEvidenceWindow)
	first := 0
	for first < len(samples) && samples[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		samples = append([]gridSample(nil), samples[first:]...)
	}
	if len(samples) >= gridEvidenceCapacity {
		samples = append([]gridSample(nil), samples[1:]...)
	}
	samples = append(samples, gridSample{grid: grid, at: now})
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
	m.evidence[nodeID] = samples
}

func (m *Manager) Candidates(nodeID uint8, now time.Time) []GridCandidate {
	if m == nil {
		return []GridCandidate{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return candidates(m.evidence[nodeID], now)
}

func candidates(samples []gridSample, now time.Time) []GridCandidate {
	type arrivals struct {
		grid  Grid
		times []time.Time
	}
	groups := make([]arrivals, 0, 4)
	cutoff := now.Add(-gridEvidenceWindow)
	for _, sample := range samples {
		if sample.at.Before(cutoff) || sample.at.After(now) {
			continue
		}
		index := -1
		for i := range groups {
			if groups[i].grid == sample.grid {
				index = i
				break
			}
		}
		if index < 0 {
			groups = append(groups, arrivals{grid: sample.grid})
			index = len(groups) - 1
		}
		groups[index].times = append(groups[index].times, sample.at)
	}
	result := make([]GridCandidate, 0, len(groups))
	for _, group := range groups {
		if len(group.times) == 0 {
			continue
		}
		sort.Slice(group.times, func(i, j int) bool { return group.times[i].Before(group.times[j]) })
		first, last := group.times[0], group.times[len(group.times)-1]
		span := last.Sub(first)
		maxGap := now.Sub(last)
		for i := 1; i < len(group.times); i++ {
			if gap := group.times[i].Sub(group.times[i-1]); gap > maxGap {
				maxGap = gap
			}
		}
		rate := 0.0
		if span > 0 {
			rate = float64(len(group.times)-1) / span.Seconds()
		}
		evidence := GridEvidence{
			SampleCount: len(group.times), SpanS: span.Seconds(), RateHz: rate,
			MaxGapS: maxGap.Seconds(), LatestAgeS: now.Sub(last).Seconds(),
		}
		eligible := group.grid.PPDUType <= 3 && span >= gridMinimumSpan && rate >= gridMinimumRateHz && maxGap < gridMaximumGap
		result = append(result, GridCandidate{Grid: group.grid, Evidence: evidence, Eligible: eligible})
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.Grid.NSubcarriers != right.Grid.NSubcarriers {
			return left.Grid.NSubcarriers > right.Grid.NSubcarriers
		}
		if left.Evidence.MaxGapS != right.Evidence.MaxGapS {
			return left.Evidence.MaxGapS < right.Evidence.MaxGapS
		}
		if left.Evidence.RateHz != right.Evidence.RateHz {
			return left.Evidence.RateHz > right.Evidence.RateHz
		}
		return left.Grid.PPDUType < right.Grid.PPDUType
	})
	return result
}

func chooseGrid(candidates []GridCandidate) (GridCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.Eligible {
			return candidate, true
		}
	}
	return GridCandidate{}, false
}

func (m *Manager) Start(request StartRequest, explicitSource *uint8) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("calibration manager is unavailable")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validDigest(request.BindingDigest) || !validNodeIDs(request.SourceNodeIDs) {
		return nil, ErrInvalidBinding
	}
	if m.session != nil && (m.session.Status == "uncalibrated" || m.session.Status == "collecting") {
		return nil, ErrAlreadyActive
	}
	if m.model != nil && (now.Before(m.model.ExpiresAt) || now.Before(m.model.CalibratedAt)) {
		return nil, ErrFreshModel
	}
	if explicitSource != nil && !containsNode(request.SourceNodeIDs, *explicitSource) {
		return nil, ErrInvalidBinding
	}
	var selected GridCandidate
	var selectedNode uint8
	found := false
	for _, nodeNumber := range request.SourceNodeIDs {
		nodeID := uint8(nodeNumber)
		if explicitSource != nil && nodeID != *explicitSource {
			continue
		}
		candidate, ok := chooseGrid(candidates(m.evidence[nodeID], now))
		if ok {
			selected, selectedNode, found = candidate, nodeID, true
			break
		}
	}
	if !found {
		if explicitSource != nil {
			return nil, fmt.Errorf("%w: node %d has no eligible raw CSI grid", ErrGridUnavailable, *explicitSource)
		}
		return nil, ErrSourceUnavailable
	}
	modelID, err := randomID("cal-model")
	if err != nil {
		return nil, fmt.Errorf("create calibration model identity: %w", err)
	}
	sessionID, err := randomID("cal-session")
	if err != nil {
		return nil, fmt.Errorf("create calibration session identity: %w", err)
	}
	if err := m.persistLocked(nil, m.dedupFactor); err != nil {
		return nil, fmt.Errorf("prepare calibration state: %w", err)
	}
	digest, nodes := request.BindingDigest, append([]int(nil), request.SourceNodeIDs...)
	m.session = &session{
		Identity: Identity{BootEpoch: m.bootEpoch, SessionID: &sessionID, BindingDigest: &digest, SourceNodeIDs: nodes},
		ModelID:  modelID, SourceNodeID: selectedNode, SourceGrid: selected.Grid,
		GridEvidence: selected.Evidence, Status: "uncalibrated", StartedAt: now,
		LastSequence: make(map[uint8]uint32), ObservedNodes: make(map[uint8]struct{}),
		LastSeen: make(map[uint8]time.Time), Reordered: make(map[uint8]uint64),
		MaxReorderDepth: make(map[uint8]uint32), SequenceFaults: make(map[uint8]struct{}),
	}
	m.model = nil
	return map[string]any{
		"success": true, "message": "Calibration started; keep the room empty while CSI frames accumulate.",
		"status": "uncalibrated", "frame_count": uint64(0), "boot_epoch": m.bootEpoch,
		"session_id": sessionID, "binding_digest": digest, "model_id": modelID,
		"source_node_ids": nodes, "source_node_id": selectedNode,
		"source_grid": selected.Grid, "grid_evidence": selected.Evidence,
	}, nil
}

func (m *Manager) Stop(identity Identity, now time.Time) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("calibration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.session
	if !identityMatches(s, identity) {
		return nil, ErrIdentityMismatch
	}
	if len(s.SequenceFaults) > 0 {
		return nil, ErrSequenceDiscontinuity
	}
	if s.Status == "calibrated" {
		if m.model == nil || !now.Before(m.model.ExpiresAt) || now.Before(m.model.CalibratedAt) {
			return nil, ErrCalibrationExpired
		}
		return map[string]any{"success": true, "model_id": s.ModelID, "model_receipt": sReceipt(s, m.model), "status": "calibrated"}, nil
	}
	missing := missingNodes(s)
	if len(missing) > 0 {
		return map[string]any{"success": false, "error_code": "calibration_source_nodes_missing", "missing_source_node_ids": missing}, ErrMissingContributors
	}
	last, ok := s.LastSeen[s.SourceNodeID]
	if !ok || now.Sub(last) < 0 || now.Sub(last) >= gridMaximumGap {
		return nil, ErrGridStale
	}
	elapsed := time.Duration(0)
	if !s.FirstFrameAt.IsZero() {
		elapsed = now.Sub(s.FirstFrameAt)
	}
	if s.FrameCount < minimumFrames || elapsed < minimumDuration {
		return map[string]any{
			"success": false, "error_code": "calibration_insufficient_evidence",
			"frame_count": s.FrameCount, "frames_needed": minimumFrames,
			"elapsed_s": elapsed.Seconds(), "duration_needed_s": minimumDuration.Seconds(),
		}, ErrInsufficientEvidence
	}
	model := &Model{
		Schema: modelSchema, ID: s.ModelID, BindingDigest: *s.Identity.BindingDigest,
		SourceNodeIDs: append([]int(nil), s.Identity.SourceNodeIDs...), SourceNodeID: s.SourceNodeID,
		SourceGrid: s.SourceGrid, FrameCount: s.FrameCount, CalibratedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(modelLifetime), Mean: make([]float64, canonicalSubcarriers),
		Variance: make([]float64, canonicalSubcarriers),
	}
	for i := range model.Mean {
		model.Mean[i] = s.Mean[i]
		if s.FrameCount > 1 {
			model.Variance[i] = s.M2[i] / float64(s.FrameCount-1)
		}
	}
	model.Receipt = Receipt{
		Schema: receiptSchema, BootEpoch: s.Identity.BootEpoch, SessionID: *s.Identity.SessionID,
		ModelID: model.ID, BindingDigest: model.BindingDigest,
		SourceNodeIDs: append([]int(nil), model.SourceNodeIDs...), SourceNodeID: model.SourceNodeID,
		SourceGrid: model.SourceGrid, FrameCount: model.FrameCount, CompletedAt: model.CalibratedAt,
	}
	if err := validateModel(model); err != nil {
		return nil, fmt.Errorf("calibration model validation failed: %w", err)
	}
	if err := m.persistLocked(model, m.dedupFactor); err != nil {
		return nil, fmt.Errorf("persist calibration model: %w", err)
	}
	m.model = model
	s.Status = "calibrated"
	return map[string]any{
		"success": true, "status": "calibrated", "frame_count": model.FrameCount,
		"elapsed_s": elapsed.Seconds(), "model_id": model.ID, "source_node_ids": model.SourceNodeIDs,
		"source_grid": model.SourceGrid, "model_receipt": model.Receipt,
		"model_kind":                     "mean_variance_reference_only",
		"calibrated_evidence_authorized": false, "numeric_vitals_authorized": false,
	}, nil
}

func sReceipt(s *session, model *Model) any {
	if model != nil {
		return model.Receipt
	}
	return nil
}

func (m *Manager) Cancel(identity Identity) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("calibration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !identityMatches(m.session, identity) {
		return nil, ErrIdentityMismatch
	}
	if m.session.Status != "uncalibrated" && m.session.Status != "collecting" {
		return nil, ErrNotCollecting
	}
	id := *m.session.Identity.SessionID
	m.session.Status = "cancelled"
	m.session = nil
	return map[string]any{"success": true, "status": "none", "cancelled_session_id": id, "frame_count": uint64(0)}, nil
}

func (m *Manager) Reset(identity Identity) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("calibration manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bootOnly := identity.BootEpoch == m.bootEpoch && identity.SessionID == nil && identity.BindingDigest == nil && len(identity.SourceNodeIDs) == 0
	if !bootOnly && !identityMatches(m.session, identity) {
		return nil, ErrIdentityMismatch
	}
	if err := m.persistLocked(nil, m.dedupFactor); err != nil {
		return nil, fmt.Errorf("persist calibration reset: %w", err)
	}
	m.session, m.model = nil, nil
	return map[string]any{"success": true, "message": "Calibration model reset.", "status": "none", "bootstrap_removed": false}, nil
}

func (m *Manager) Status(now time.Time) map[string]any {
	if m == nil {
		return map[string]any{"active": false, "calibrated": false, "status": "unavailable", "numeric_vitals_authorized": false, "calibrated_evidence_authorized": false}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := map[string]any{
		"active": false, "calibrated": false, "status": "none", "boot_epoch": m.bootEpoch,
		"session_id": nil, "binding_digest": nil, "model_id": nil, "source_node_ids": []int{},
		"observed_source_node_ids": []int{}, "missing_source_node_ids": []int{},
		"last_sequence_by_node": map[string]uint32{}, "reordered_packets_by_node": map[string]uint64{},
		"max_reorder_depth_by_node": map[string]uint32{}, "sequence_fault_node_ids": []int{},
		"sequence_policy":          "forward_only_with_bounded_udp_reorder_drop_v1",
		"reorder_window_sequences": maximumReorderDepth,
		"min_frames":               minimumFrames, "min_duration_s": minimumDuration.Seconds(),
		"binding_mode": "none", "calibrated_evidence_authorized": false,
		"numeric_vitals_authorized": false, "spatial_localization_available": false,
		"model_kind": "mean_variance_reference_only",
	}
	if m.session != nil {
		s := m.session
		observed := sortedKeys(s.ObservedNodes)
		faults := sortedKeys(s.SequenceFaults)
		missing := missingNodes(s)
		last := make(map[string]uint32, len(s.LastSequence))
		reordered := make(map[string]uint64, len(s.Reordered))
		depth := make(map[string]uint32, len(s.MaxReorderDepth))
		for id, sequence := range s.LastSequence {
			last[fmt.Sprintf("%d", id)] = sequence
		}
		for id, count := range s.Reordered {
			reordered[fmt.Sprintf("%d", id)] = count
		}
		for id, value := range s.MaxReorderDepth {
			depth[fmt.Sprintf("%d", id)] = value
		}
		elapsed := time.Duration(0)
		if !s.FirstFrameAt.IsZero() {
			elapsed = now.Sub(s.FirstFrameAt)
			if elapsed < 0 {
				elapsed = 0
			}
		}
		fps := 0.0
		if elapsed > 0 {
			fps = float64(s.FrameCount) / elapsed.Seconds()
		}
		latestAge := any(nil)
		if seen, ok := s.LastSeen[s.SourceNodeID]; ok {
			age := max(0, now.Sub(seen).Milliseconds())
			latestAge = age
		}
		status["active"], status["status"], status["binding_mode"] = true, s.Status, "bound"
		status["session_id"], status["binding_digest"], status["model_id"] = s.Identity.SessionID, s.Identity.BindingDigest, s.ModelID
		status["source_node_ids"], status["source_node_id"], status["source_grid"] = append([]int(nil), s.Identity.SourceNodeIDs...), s.SourceNodeID, s.SourceGrid
		status["grid_evidence"], status["grid_latest_seen_ms"] = s.GridEvidence, latestAge
		status["observed_source_node_ids"], status["missing_source_node_ids"] = observed, missing
		status["last_sequence_by_node"], status["reordered_packets_by_node"], status["max_reorder_depth_by_node"] = last, reordered, depth
		status["sequence_fault_node_ids"] = faults
		status["frame_count"], status["elapsed_s"], status["frames_per_second"] = s.FrameCount, elapsed.Seconds(), fps
	}
	if m.model != nil {
		fresh := now.Before(m.model.ExpiresAt) && !now.Before(m.model.CalibratedAt)
		status["calibrated"] = fresh
		if m.session == nil {
			status["status"] = "stored"
			if !fresh {
				status["status"] = "expired"
			}
		}
		status["model_id"], status["model_receipt"], status["model_expires_at"] = m.model.ID, m.model.Receipt, m.model.ExpiresAt
		status["calibrated_at"] = m.model.CalibratedAt
		status["model_frame_count"] = m.model.FrameCount
	}
	return status
}

func (m *Manager) Trust(occupants *uint8, signalQuality *float64, vitalConfidence *float64, now time.Time) map[string]any {
	if m == nil {
		return map[string]any{"authority": "abstained", "authorized": false, "reasons": []string{"calibration_manager_unavailable"}}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	reasons := make([]string, 0, 6)
	if m.model == nil || !now.Before(m.model.ExpiresAt) || now.Before(m.model.CalibratedAt) {
		reasons = append(reasons, "fresh_calibration_required")
	}
	if m.session == nil || m.session.Status != "calibrated" {
		reasons = append(reasons, "live_calibration_binding_unavailable")
	} else {
		if len(m.session.SequenceFaults) != 0 {
			reasons = append(reasons, "csi_sequence_discontinuity")
		}
		stale := false
		for _, nodeID := range m.session.Identity.SourceNodeIDs {
			seen := m.session.LastSeen[uint8(nodeID)]
			if seen.IsZero() || now.Sub(seen) < 0 || now.Sub(seen) > 5*time.Second {
				stale = true
				break
			}
		}
		if stale {
			reasons = append(reasons, "bound_device_csi_stale")
		}
	}
	if occupants == nil || *occupants != 1 {
		reasons = append(reasons, "exactly_one_occupant_required")
	}
	if signalQuality == nil || math.IsNaN(*signalQuality) || *signalQuality < 0.40 {
		reasons = append(reasons, "signal_quality_below_threshold")
	}
	if vitalConfidence == nil || math.IsNaN(*vitalConfidence) || *vitalConfidence < 0.55 {
		reasons = append(reasons, "vital_confidence_below_threshold")
	}
	// This Go migration has not ported the Rust calibrated holdout scorer. A
	// baseline alone is not evidence of a person or a vital-sign measurement.
	reasons = append(reasons, "calibrated_inference_not_available")
	return map[string]any{
		"authority": "abstained", "authorized": false, "reasons": reasons,
		"calibrated_evidence_authorized": false, "numeric_vitals_authorized": false,
	}
}

func (m *Manager) DedupFactor() float64 {
	if m == nil {
		return defaultDedupFactor
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dedupFactor
}

func (m *Manager) SetDedupFactor(value float64) (float64, error) {
	if m == nil {
		return defaultDedupFactor, errors.New("calibration manager is unavailable")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("dedup factor must be finite")
	}
	value = min(max(value, 1), 10)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.persistLocked(m.model, value); err != nil {
		return m.dedupFactor, fmt.Errorf("persist dedup factor: %w", err)
	}
	m.dedupFactor = value
	return value, nil
}

func GroundTruthFactor(rawSum, count uint64) (float64, error) {
	if count == 0 {
		return 0, ErrInvalidGroundTruth
	}
	value := defaultDedupFactor
	if rawSum > 0 {
		value = float64(rawSum) / float64(count)
	}
	return min(max(value, 1), 10), nil
}

func (m *Manager) SetGroundTruth(rawSum, count uint64) (float64, error) {
	factor, err := GroundTruthFactor(rawSum, count)
	if err != nil {
		return 0, err
	}
	return m.SetDedupFactor(factor)
}

func (m *Manager) load() error {
	f, err := os.Open(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open calibration state: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maximumStateFileBytes+1))
	if err != nil {
		return fmt.Errorf("read calibration state: %w", err)
	}
	if len(data) > maximumStateFileBytes {
		return errors.New("calibration state file exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state persistedState
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("decode calibration state: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("calibration state must contain one JSON object")
	}
	if state.Schema != persistSchema || !finiteBetween(state.DedupFactor, 1, 10) {
		return errors.New("calibration state schema or dedup factor is invalid")
	}
	if state.Model != nil {
		if err := validateModel(state.Model); err != nil {
			return fmt.Errorf("validate stored calibration model: %w", err)
		}
		copy := cloneModel(state.Model)
		m.model = &copy
	}
	m.dedupFactor = state.DedupFactor
	return nil
}

func (m *Manager) persistLocked(model *Model, factor float64) error {
	state := persistedState{Schema: persistSchema, DedupFactor: factor}
	if model != nil {
		copy := cloneModel(model)
		state.Model = &copy
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maximumStateFileBytes {
		return errors.New("calibration state exceeds size limit")
	}
	directory := filepath.Dir(m.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create calibration state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".calibration-state-*")
	if err != nil {
		return fmt.Errorf("create calibration state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure calibration state file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write calibration state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync calibration state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close calibration state: %w", err)
	}
	if err := os.Rename(temporaryPath, m.path); err != nil {
		return fmt.Errorf("replace calibration state: %w", err)
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		defer directoryHandle.Close()
		if err := directoryHandle.Sync(); err != nil {
			return fmt.Errorf("sync calibration state directory: %w", err)
		}
	}
	return nil
}

func validateModel(model *Model) error {
	if model == nil || model.Schema != modelSchema || !validOpaqueID(model.ID, "cal-model-") || !validDigest(model.BindingDigest) || !validNodeIDs(model.SourceNodeIDs) || !containsNode(model.SourceNodeIDs, model.SourceNodeID) || model.SourceGrid.NSubcarriers == 0 || model.SourceGrid.PPDUType > 3 || model.FrameCount < minimumFrames || model.CalibratedAt.IsZero() || !model.ExpiresAt.After(model.CalibratedAt) || len(model.Mean) != canonicalSubcarriers || len(model.Variance) != canonicalSubcarriers {
		return errors.New("calibration model metadata is invalid")
	}
	if model.Receipt.Schema != receiptSchema || !validOpaqueID(model.Receipt.BootEpoch, "boot-") || !validOpaqueID(model.Receipt.SessionID, "cal-session-") || model.Receipt.ModelID != model.ID || model.Receipt.BindingDigest != model.BindingDigest || model.Receipt.SourceNodeID != model.SourceNodeID || model.Receipt.SourceGrid != model.SourceGrid || model.Receipt.FrameCount != model.FrameCount || model.Receipt.CompletedAt.IsZero() || !model.Receipt.CompletedAt.Equal(model.CalibratedAt) || !sameNodes(model.Receipt.SourceNodeIDs, model.SourceNodeIDs) {
		return errors.New("calibration model receipt is invalid")
	}
	for i := range model.Mean {
		if !finiteBetween(model.Mean[i], 0, math.MaxFloat64) || !finiteBetween(model.Variance[i], 0, math.MaxFloat64) {
			return fmt.Errorf("calibration statistics contain an invalid value at tone %d", i)
		}
	}
	return nil
}

func cloneModel(model *Model) Model {
	copy := *model
	copy.SourceNodeIDs = append([]int(nil), model.SourceNodeIDs...)
	copy.Mean = append([]float64(nil), model.Mean...)
	copy.Variance = append([]float64(nil), model.Variance...)
	copy.Receipt.SourceNodeIDs = append([]int(nil), model.Receipt.SourceNodeIDs...)
	return copy
}

func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validOpaqueID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func validNodeIDs(nodes []int) bool {
	if len(nodes) == 0 || len(nodes) > 16 {
		return false
	}
	for _, nodeID := range nodes {
		if nodeID < 0 || nodeID > math.MaxUint8 {
			return false
		}
	}
	for i := 1; i < len(nodes); i++ {
		if nodes[i-1] >= nodes[i] {
			return false
		}
	}
	return true
}

func containsNode(nodes []int, id uint8) bool {
	i := sort.Search(len(nodes), func(i int) bool { return nodes[i] >= int(id) })
	return i < len(nodes) && nodes[i] == int(id)
}

func sameNodes(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func identityMatches(s *session, identity Identity) bool {
	return s != nil && identity.BootEpoch == s.Identity.BootEpoch && identity.SessionID != nil && s.Identity.SessionID != nil && *identity.SessionID == *s.Identity.SessionID && identity.BindingDigest != nil && s.Identity.BindingDigest != nil && *identity.BindingDigest == *s.Identity.BindingDigest && validNodeIDs(identity.SourceNodeIDs) && sameNodes(identity.SourceNodeIDs, s.Identity.SourceNodeIDs)
}

func missingNodes(s *session) []int {
	missing := make([]int, 0)
	for _, nodeID := range s.Identity.SourceNodeIDs {
		if _, ok := s.ObservedNodes[uint8(nodeID)]; !ok {
			missing = append(missing, nodeID)
		}
	}
	return missing
}

func sortedKeys(values map[uint8]struct{}) []int {
	result := make([]int, 0, len(values))
	for id := range values {
		result = append(result, int(id))
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func finiteBetween(value, low, high float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= low && value <= high
}

func randomID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(bytes[:]), nil
}
