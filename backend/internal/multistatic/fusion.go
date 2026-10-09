// Package multistatic fuses time-aligned CSI amplitude views from multiple
// sensing nodes. It deliberately keeps RF phase separate unless every input
// declares the same shared phase reference.
package multistatic

import (
	"errors"
	"math"
	"sort"
	"time"
)

const (
	CanonicalSubcarriers          = 56
	DefaultGuardIntervalUS uint64 = 60_000
	DefaultSoftGuardUS     uint64 = 20_000
)

var ErrTimestampSpread = errors.New("CSI timestamp spread exceeds the fusion guard")

type PhaseReference string

const (
	PhaseUnavailable PhaseReference = "unavailable"
	PhaseNodeLocal   PhaseReference = "node_local"
	PhaseShared      PhaseReference = "shared"
)

type Frame struct {
	NodeID             uint8
	Sequence           uint32
	Antennas           int
	Subcarriers        int
	Amplitude          []float64
	Phase              []float64
	PhaseReference     PhaseReference
	PhaseReferenceID   uint32
	MeshTimestampUS    *uint64
	TimestampUS        uint64
	ArrivalTimestampUS uint64
	SeenAt             time.Time
	Position           [3]float64
	PositionAvailable  bool
	Source             string
	InputMode          string
}

type PhaseFusion struct {
	Mode        string  `json:"mode"`
	Reason      string  `json:"reason,omitempty"`
	ReferenceID *uint32 `json:"reference_id,omitempty"`
}

type Contributor struct {
	NodeID            uint8      `json:"node_id"`
	Sequence          uint32     `json:"sequence"`
	TimestampUS       uint64     `json:"timestamp_us"`
	AgeMS             uint64     `json:"age_ms"`
	Position          [3]float64 `json:"position"`
	PositionAvailable bool       `json:"position_available"`
	AttentionWeight   float64    `json:"attention_weight"`
}

type Snapshot struct {
	Status                       string        `json:"status"`
	Reason                       string        `json:"reason,omitempty"`
	Available                    bool          `json:"available"`
	Fused                        bool          `json:"fused"`
	TimeAlignment                string        `json:"time_alignment"`
	AlignmentQuality             string        `json:"alignment_quality"`
	Synchronized                 bool          `json:"synchronized"`
	TimestampUS                  *uint64       `json:"timestamp_us,omitempty"`
	TimestampSpreadUS            uint64        `json:"timestamp_spread_us"`
	GuardIntervalUS              uint64        `json:"guard_interval_us"`
	SoftGuardIntervalUS          uint64        `json:"soft_guard_interval_us"`
	CandidateNodes               int           `json:"candidate_nodes"`
	ActiveNodes                  int           `json:"active_nodes"`
	RequiredNodes                int           `json:"required_nodes"`
	ContributorNodeIDs           []int         `json:"contributor_node_ids"`
	OutOfWindowNodeIDs           []int         `json:"out_of_window_node_ids"`
	RejectedNodeIDs              []int         `json:"rejected_node_ids"`
	ConfiguredPositionNodes      int           `json:"configured_position_nodes"`
	GeometryConfigured           bool          `json:"geometry_configured"`
	Contributors                 []Contributor `json:"contributors"`
	FusedAmplitude               []float64     `json:"fused_amplitude"`
	FusedPhase                   []float64     `json:"-"`
	CrossNodeCoherence           float64       `json:"cross_node_coherence"`
	PhaseFusion                  PhaseFusion   `json:"phase_fusion"`
	Evidence                     string        `json:"evidence"`
	SpatialLocalizationAvailable bool          `json:"spatial_localization_available"`
}

type Options struct {
	GuardIntervalUS      uint64
	SoftGuardUS          uint64
	StaleAfter           time.Duration
	AttentionTemperature float64
}

func DefaultOptions() Options {
	return Options{
		GuardIntervalUS:      DefaultGuardIntervalUS,
		SoftGuardUS:          DefaultSoftGuardUS,
		StaleAfter:           5 * time.Second,
		AttentionTemperature: 1,
	}
}

func EmptySnapshot() Snapshot {
	return Snapshot{
		Status: "unavailable", Reason: "no_fresh_raw_csi_frames",
		TimeAlignment: "unavailable", GuardIntervalUS: DefaultGuardIntervalUS,
		AlignmentQuality: "unavailable", SoftGuardIntervalUS: DefaultSoftGuardUS,
		RequiredNodes: 2, ContributorNodeIDs: []int{}, OutOfWindowNodeIDs: []int{},
		RejectedNodeIDs: []int{}, Contributors: []Contributor{}, FusedAmplitude: []float64{},
		FusedPhase: []float64{}, PhaseFusion: PhaseFusion{Mode: "amplitude_only", Reason: "phase_unavailable"},
		Evidence: "unverified", SpatialLocalizationAvailable: false,
	}
}

// FuseLatest reproduces the Rust bridge's frame-domain selection: use mesh
// epoch time only when every fresh node has a valid aligned timestamp and the
// complete cohort fits the guard. Otherwise, compare host-arrival timestamps
// consistently for all nodes, clearly labeling that estimate as unsynchronized.
func FuseLatest(frames []Frame, now time.Time, options Options) Snapshot {
	options = normalizeOptions(options)
	snapshot := EmptySnapshot()
	snapshot.GuardIntervalUS = options.GuardIntervalUS
	if len(frames) == 0 {
		return snapshot
	}

	latestByNode := make(map[uint8]Frame, len(frames))
	rejected := make(map[int]struct{})
	synthetic := false
	for _, frame := range frames {
		if frame.Source == "wifi" || frame.InputMode == "rssi_derived" || (frame.Source != "" && frame.Source != "esp32" && frame.Source != "simulate" && frame.Source != "synthetic") {
			rejected[int(frame.NodeID)] = struct{}{}
			continue
		}
		if frame.SeenAt.IsZero() {
			rejected[int(frame.NodeID)] = struct{}{}
			continue
		}
		age := now.Sub(frame.SeenAt)
		if age < 0 {
			age = 0
		}
		if age > options.StaleAfter {
			continue
		}
		if frame.Source == "simulate" || frame.Source == "synthetic" {
			synthetic = true
		}
		amplitude, phase, phaseReference, err := canonicalize(frame)
		if err != nil {
			rejected[int(frame.NodeID)] = struct{}{}
			continue
		}
		frame.Amplitude, frame.Phase, frame.PhaseReference = amplitude, phase, phaseReference
		if prior, ok := latestByNode[frame.NodeID]; !ok || newer(frame, prior) {
			latestByNode[frame.NodeID] = frame
		}
	}
	for id := range rejected {
		snapshot.RejectedNodeIDs = append(snapshot.RejectedNodeIDs, id)
	}
	sort.Ints(snapshot.RejectedNodeIDs)
	if len(latestByNode) == 0 {
		if len(snapshot.RejectedNodeIDs) > 0 {
			snapshot.Reason = "no_usable_raw_csi_frames"
		}
		return snapshot
	}
	if synthetic {
		snapshot.Evidence = "synthetic"
	}

	active := make([]Frame, 0, len(latestByNode))
	for _, frame := range latestByNode {
		active = append(active, frame)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].NodeID < active[j].NodeID })
	snapshot.CandidateNodes = len(active)

	meshTimestamps := make([]uint64, 0, len(active))
	allMeshAligned := true
	for _, frame := range active {
		if frame.MeshTimestampUS == nil {
			allMeshAligned = false
			break
		}
		meshTimestamps = append(meshTimestamps, *frame.MeshTimestampUS)
	}
	if allMeshAligned && timestampSpread(meshTimestamps) <= options.GuardIntervalUS {
		snapshot.TimeAlignment = "mesh_epoch"
		snapshot.Synchronized = true
	} else {
		snapshot.TimeAlignment = "host_arrival_estimate"
	}

	for i := range active {
		if snapshot.Synchronized {
			active[i].TimestampUS = *active[i].MeshTimestampUS
		} else {
			active[i].TimestampUS = active[i].ArrivalTimestampUS
		}
	}
	freshest := active[0].TimestampUS
	for _, frame := range active[1:] {
		if frame.TimestampUS > freshest {
			freshest = frame.TimestampUS
		}
	}
	cohort := make([]Frame, 0, len(active))
	for _, frame := range active {
		if freshest-frame.TimestampUS > options.GuardIntervalUS {
			snapshot.OutOfWindowNodeIDs = append(snapshot.OutOfWindowNodeIDs, int(frame.NodeID))
			continue
		}
		cohort = append(cohort, frame)
	}
	snapshot.CandidateNodes = len(active)
	snapshot.ActiveNodes = len(cohort)

	result, err := fuseCohort(cohort, now, options)
	if err != nil {
		snapshot.Reason = "timestamp_mismatch"
		return snapshot
	}
	result.TimeAlignment = snapshot.TimeAlignment
	result.Synchronized = snapshot.Synchronized
	result.CandidateNodes = snapshot.CandidateNodes
	result.OutOfWindowNodeIDs = snapshot.OutOfWindowNodeIDs
	result.RejectedNodeIDs = snapshot.RejectedNodeIDs
	result.Evidence = snapshot.Evidence
	return result
}

func normalizeOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.GuardIntervalUS == 0 {
		options.GuardIntervalUS = defaults.GuardIntervalUS
	}
	if options.SoftGuardUS == 0 || options.SoftGuardUS >= options.GuardIntervalUS {
		options.SoftGuardUS = min(defaults.SoftGuardUS, options.GuardIntervalUS-1)
	}
	if options.StaleAfter <= 0 {
		options.StaleAfter = defaults.StaleAfter
	}
	if !finite(options.AttentionTemperature) || options.AttentionTemperature <= 0 {
		options.AttentionTemperature = defaults.AttentionTemperature
	}
	return options
}

func newer(candidate, current Frame) bool {
	if candidate.ArrivalTimestampUS != current.ArrivalTimestampUS {
		return candidate.ArrivalTimestampUS > current.ArrivalTimestampUS
	}
	return candidate.Sequence > current.Sequence
}

func fuseCohort(frames []Frame, now time.Time, options Options) (Snapshot, error) {
	if len(frames) == 0 {
		return EmptySnapshot(), errors.New("no frames")
	}
	minTS, maxTS := frames[0].TimestampUS, frames[0].TimestampUS
	for _, frame := range frames[1:] {
		if frame.TimestampUS < minTS {
			minTS = frame.TimestampUS
		}
		if frame.TimestampUS > maxTS {
			maxTS = frame.TimestampUS
		}
	}
	spread := maxTS - minTS
	if spread > options.GuardIntervalUS {
		return EmptySnapshot(), ErrTimestampSpread
	}

	weights := attentionWeights(frames, options.AttentionTemperature)
	fusedAmplitude := make([]float64, CanonicalSubcarriers)
	for n, frame := range frames {
		for i, value := range frame.Amplitude {
			fusedAmplitude[i] += weights[n] * value
		}
	}
	phaseFusion := phaseFusionFor(frames)
	fusedPhase := fusePhase(frames, weights, phaseFusion)

	snapshot := EmptySnapshot()
	snapshot.Status = "fused"
	snapshot.Reason = ""
	snapshot.Available = true
	snapshot.Fused = len(frames) >= 2
	if len(frames) == 1 {
		snapshot.Status = "single_node"
		snapshot.Reason = "at_least_two_nodes_required_for_multistatic_fusion"
	}
	snapshot.TimeAlignment = "unavailable"
	snapshot.GuardIntervalUS = options.GuardIntervalUS
	snapshot.SoftGuardIntervalUS = options.SoftGuardUS
	if spread > options.SoftGuardUS {
		snapshot.AlignmentQuality = "loose"
	} else {
		snapshot.AlignmentQuality = "tight"
	}
	snapshot.TimestampSpreadUS = spread
	snapshot.CandidateNodes = len(frames)
	snapshot.ActiveNodes = len(frames)
	snapshot.RequiredNodes = 2
	snapshot.FusedAmplitude = fusedAmplitude
	snapshot.FusedPhase = fusedPhase
	snapshot.CrossNodeCoherence = weightEntropy(weights)
	snapshot.PhaseFusion = phaseFusion
	snapshot.SpatialLocalizationAvailable = false
	if len(frames) > 0 {
		timestamps := make([]uint64, len(frames))
		for i, frame := range frames {
			timestamps[i] = frame.TimestampUS
		}
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
		timestamp := timestamps[len(timestamps)/2]
		snapshot.TimestampUS = &timestamp
	}
	for i, frame := range frames {
		age := now.Sub(frame.SeenAt)
		if age < 0 {
			age = 0
		}
		if frame.PositionAvailable {
			snapshot.ConfiguredPositionNodes++
		}
		snapshot.ContributorNodeIDs = append(snapshot.ContributorNodeIDs, int(frame.NodeID))
		snapshot.Contributors = append(snapshot.Contributors, Contributor{
			NodeID: frame.NodeID, Sequence: frame.Sequence, TimestampUS: frame.TimestampUS,
			AgeMS: uint64(age.Milliseconds()), Position: frame.Position,
			PositionAvailable: frame.PositionAvailable, AttentionWeight: weights[i],
		})
	}
	// One known sensor position is useful metadata, but it does not define a
	// multistatic geometry. Require at least two positioned contributors.
	snapshot.GeometryConfigured = len(frames) >= 2 && snapshot.ConfiguredPositionNodes == len(frames)
	return snapshot, nil
}

func canonicalize(frame Frame) ([]float64, []float64, PhaseReference, error) {
	if frame.Subcarriers <= 0 || len(frame.Amplitude) == 0 || len(frame.Amplitude)%frame.Subcarriers != 0 {
		return nil, nil, PhaseUnavailable, errors.New("invalid amplitude dimensions")
	}
	chains := len(frame.Amplitude) / frame.Subcarriers
	if chains == 0 || (frame.Antennas > 0 && chains != frame.Antennas) {
		return nil, nil, PhaseUnavailable, errors.New("antenna count does not match CSI dimensions")
	}
	perTone := make([]float64, frame.Subcarriers)
	for tone := 0; tone < frame.Subcarriers; tone++ {
		for chain := 0; chain < chains; chain++ {
			value := frame.Amplitude[chain*frame.Subcarriers+tone]
			if !finite(value) || value < 0 {
				return nil, nil, PhaseUnavailable, errors.New("amplitude must be finite and non-negative")
			}
			perTone[tone] += value / float64(chains)
		}
	}
	canonicalAmplitude := resampleCubic(perTone, CanonicalSubcarriers)

	phaseReference := frame.PhaseReference
	if phaseReference != PhaseNodeLocal && phaseReference != PhaseShared {
		phaseReference = PhaseUnavailable
	}
	if phaseReference == PhaseUnavailable || len(frame.Phase) != len(frame.Amplitude) {
		return canonicalAmplitude, nil, PhaseUnavailable, nil
	}
	perTonePhase := make([]float64, frame.Subcarriers)
	for tone := 0; tone < frame.Subcarriers; tone++ {
		value := frame.Phase[tone] // One receiver chain is enough for a phase trace.
		if !finite(value) {
			return canonicalAmplitude, nil, PhaseUnavailable, nil
		}
		perTonePhase[tone] = value
	}
	canonicalPhase := resampleCubic(unwrapPhase(perTonePhase), CanonicalSubcarriers)
	for i := range canonicalPhase {
		canonicalPhase[i] = math.Remainder(canonicalPhase[i], 2*math.Pi)
	}
	return canonicalAmplitude, canonicalPhase, phaseReference, nil
}

func attentionWeights(frames []Frame, temperature float64) []float64 {
	weights := make([]float64, len(frames))
	if len(frames) == 0 {
		return weights
	}
	if len(frames) == 1 {
		weights[0] = 1
		return weights
	}
	consensus := make([]float64, CanonicalSubcarriers)
	for _, frame := range frames {
		for i, value := range frame.Amplitude {
			consensus[i] += value / float64(len(frames))
		}
	}
	logits := make([]float64, len(frames))
	maxLogit := math.Inf(-1)
	for n, frame := range frames {
		dot, normA, normB := 0.0, 0.0, 0.0
		for i, value := range frame.Amplitude {
			dot += value * consensus[i]
			normA += value * value
			normB += consensus[i] * consensus[i]
		}
		denom := math.Max(math.Sqrt(normA*normB), 1e-12)
		logits[n] = (dot / denom) / temperature
		if logits[n] > maxLogit {
			maxLogit = logits[n]
		}
	}
	sum := 0.0
	for i, logit := range logits {
		weights[i] = math.Exp(logit - maxLogit)
		sum += weights[i]
	}
	if sum <= 0 || !finite(sum) {
		for i := range weights {
			weights[i] = 1 / float64(len(weights))
		}
		return weights
	}
	for i := range weights {
		weights[i] /= sum
	}
	return weights
}

func phaseFusionFor(frames []Frame) PhaseFusion {
	if len(frames) == 0 {
		return PhaseFusion{Mode: "amplitude_only", Reason: "phase_unavailable"}
	}
	for _, frame := range frames {
		if frame.PhaseReference == PhaseUnavailable || len(frame.Phase) != CanonicalSubcarriers {
			return PhaseFusion{Mode: "amplitude_only", Reason: "phase_unavailable"}
		}
	}
	if len(frames) == 1 {
		return PhaseFusion{Mode: "single_node"}
	}
	first := frames[0].PhaseReference
	if first != PhaseShared {
		return PhaseFusion{Mode: "amplitude_only", Reason: "independent_clocks"}
	}
	referenceID := frames[0].PhaseReferenceID
	for _, frame := range frames[1:] {
		if frame.PhaseReference != PhaseShared {
			return PhaseFusion{Mode: "amplitude_only", Reason: "independent_clocks"}
		}
		if frame.PhaseReferenceID != referenceID {
			return PhaseFusion{Mode: "amplitude_only", Reason: "mixed_references"}
		}
	}
	return PhaseFusion{Mode: "coherent", ReferenceID: &referenceID}
}

func fusePhase(frames []Frame, weights []float64, mode PhaseFusion) []float64 {
	if mode.Mode == "amplitude_only" || len(frames) == 0 {
		return []float64{}
	}
	if len(frames) == 1 {
		return append([]float64(nil), frames[0].Phase...)
	}
	phase := make([]float64, CanonicalSubcarriers)
	for i := range phase {
		sinSum, cosSum := 0.0, 0.0
		for n, frame := range frames {
			sinSum += weights[n] * math.Sin(frame.Phase[i])
			cosSum += weights[n] * math.Cos(frame.Phase[i])
		}
		phase[i] = math.Atan2(sinSum, cosSum)
	}
	return phase
}

func weightEntropy(weights []float64) float64 {
	if len(weights) <= 1 {
		return 1
	}
	maxEntropy := math.Log(float64(len(weights)))
	if maxEntropy <= 1e-12 {
		return 1
	}
	entropy := 0.0
	for _, weight := range weights {
		if weight > 1e-12 {
			entropy -= weight * math.Log(weight)
		}
	}
	return clamp(entropy/maxEntropy, 0, 1)
}

func resampleCubic(src []float64, dstLen int) []float64 {
	if dstLen <= 0 {
		return []float64{}
	}
	if len(src) == dstLen {
		return append([]float64(nil), src...)
	}
	if len(src) == 0 {
		return make([]float64, dstLen)
	}
	if len(src) == 1 {
		out := make([]float64, dstLen)
		for i := range out {
			out[i] = src[0]
		}
		return out
	}
	ratio := float64(len(src)-1) / float64(max(dstLen-1, 1))
	out := make([]float64, dstLen)
	for i := range out {
		x := float64(i) * ratio
		idx := int(math.Floor(x))
		t := x - float64(idx)
		p0, p1 := src[clampIndex(idx-1, len(src))], src[clampIndex(idx, len(src))]
		p2, p3 := src[clampIndex(idx+1, len(src))], src[clampIndex(idx+2, len(src))]
		a := -0.5*p0 + 1.5*p1 - 1.5*p2 + 0.5*p3
		b := p0 - 2.5*p1 + 2*p2 - 0.5*p3
		c := -0.5*p0 + 0.5*p2
		out[i] = a*t*t*t + b*t*t + c*t + p1
	}
	return out
}

func unwrapPhase(values []float64) []float64 {
	if len(values) == 0 {
		return []float64{}
	}
	out := append([]float64(nil), values...)
	for i := 1; i < len(out); i++ {
		delta := out[i] - out[i-1]
		if delta > math.Pi {
			out[i] -= 2 * math.Pi * math.Ceil((delta-math.Pi)/(2*math.Pi))
		} else if delta < -math.Pi {
			out[i] += 2 * math.Pi * math.Ceil((-math.Pi-delta)/(2*math.Pi))
		}
	}
	return out
}

func timestampSpread(timestamps []uint64) uint64 {
	if len(timestamps) < 2 {
		return 0
	}
	minTS, maxTS := timestamps[0], timestamps[0]
	for _, timestamp := range timestamps[1:] {
		if timestamp < minTS {
			minTS = timestamp
		}
		if timestamp > maxTS {
			maxTS = timestamp
		}
	}
	return maxTS - minTS
}

func clampIndex(index, length int) int {
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func clamp(value, low, high float64) float64 {
	if !finite(value) {
		return low
	}
	return math.Max(low, math.Min(value, high))
}
