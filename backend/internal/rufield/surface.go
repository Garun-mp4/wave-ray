// Package rufield bridges aggregate Go sensing updates to the RuField MFS
// event schema. It exposes only the P2 anonymous occupancy/motion projection.
package rufield

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ruvnet/ruview/backend/internal/signal"
	"github.com/ruvnet/ruview/backend/internal/state"
	stream "github.com/ruvnet/ruview/backend/internal/ws"
)

const (
	SpecVersion     = "rufield.mfs.v0.1"
	ModelID         = "ruview_sensing_server_v1"
	RingCapacity    = 64
	SigningSeedFile = "rufield_signing_seed"
)

var ErrInvalidEvent = errors.New("invalid RuField event")

type FieldTensor struct {
	SpecVersion   string    `json:"spec_version"`
	TimestampNS   uint64    `json:"timestamp_ns"`
	Modality      string    `json:"modality"`
	Axes          []string  `json:"axes"`
	Shape         []int     `json:"shape"`
	Values        []float32 `json:"values"`
	Confidence    float32   `json:"confidence"`
	NoiseFloor    float32   `json:"noise_floor"`
	CalibrationID *string   `json:"calibration_id"`
	PrivacyClass  string    `json:"privacy_class"`
}

type SensorDescriptor struct {
	Modality        string      `json:"modality"`
	Vendor          string      `json:"vendor"`
	DeviceID        string      `json:"device_id"`
	Placement       string      `json:"placement"`
	CoordinateFrame *string     `json:"coordinate_frame,omitempty"`
	PositionM       *[3]float32 `json:"position_m,omitempty"`
	OrientationXYZW *[4]float32 `json:"orientation_xyzw,omitempty"`
	ClockDomain     string      `json:"clock_domain"`
}

type Observation struct {
	ZoneID                    *string            `json:"zone_id"`
	SpaceCell                 *[3]int32          `json:"space_cell"`
	RangeM                    *float32           `json:"range_m"`
	VelocityMPS               *float32           `json:"velocity_mps"`
	MotionVector              *[3]float32        `json:"motion_vector"`
	Confidence                float32            `json:"confidence"`
	Features                  map[string]float32 `json:"features"`
	Labels                    []string           `json:"labels"`
	PrivacyClass              string             `json:"privacy_class"`
	TrackID                   *string            `json:"track_id,omitempty"`
	Attributes                map[string]string  `json:"attributes,omitempty"`
	IdentityEvidence          json.RawMessage    `json:"identity_evidence,omitempty"`
	ChannelSoundingProvenance json.RawMessage    `json:"channel_sounding_provenance,omitempty"`
}

type ProvenanceRef struct {
	RawHash         string  `json:"raw_hash"`
	FirmwareHash    string  `json:"firmware_hash"`
	ModelID         string  `json:"model_id"`
	CalibrationID   string  `json:"calibration_id"`
	Synthetic       bool    `json:"synthetic"`
	SignatureHex    *string `json:"signature_hex,omitempty"`
	SignerPubkeyHex *string `json:"signer_pubkey_hex,omitempty"`
}

type FieldEvent struct {
	SpecVersion string           `json:"spec_version"`
	EventID     string           `json:"event_id"`
	TimestampNS uint64           `json:"timestamp_ns"`
	Sensor      SensorDescriptor `json:"sensor"`
	Tensor      FieldTensor      `json:"tensor"`
	Observation Observation      `json:"observation"`
	Provenance  ProvenanceRef    `json:"provenance"`
}

type Response struct {
	Spec          string       `json:"spec"`
	Endpoint      string       `json:"endpoint"`
	SignerPubkey  string       `json:"signer_pubkey_hex"`
	DevSigningKey bool         `json:"dev_signing_key"`
	Events        []FieldEvent `json:"events"`
}

type Surface struct {
	privateKey  ed25519.PrivateKey
	publicHex   string
	usingDevKey bool

	mu     sync.RWMutex
	recent []FieldEvent
	events *stream.Hub
}

// New reads WDP_RUFIELD_SIGNING_SEED from the environment and otherwise
// creates or loads a private per-install signing key in dataDir. An invalid
// configured seed or unsafe stored key fails closed.
func New(dataDir string) (*Surface, error) {
	return NewWithSeed(dataDir, os.Getenv("WDP_RUFIELD_SIGNING_SEED"))
}

// NewWithSeed is injectable for tests. A provided seed is never written to
// disk; an absent seed is replaced with a persistent randomly-generated key.
func NewWithSeed(dataDir, configuredSeed string) (*Surface, error) {
	var seed [ed25519.SeedSize]byte
	if strings.TrimSpace(configuredSeed) != "" {
		parsed, ok := parseSeed(configuredSeed)
		if !ok {
			return nil, errors.New("WDP_RUFIELD_SIGNING_SEED must be 64 hex characters or at least 32 bytes")
		}
		seed = parsed
	} else {
		loaded, err := loadOrCreateSeed(dataDir)
		if err != nil {
			return nil, fmt.Errorf("load RuField signing key: %w", err)
		}
		seed = loaded
	}
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Surface{
		privateKey: privateKey,
		publicHex:  hex.EncodeToString(publicKey),
		recent:     make([]FieldEvent, 0, RingCapacity),
		events:     stream.NewHub(),
	}, nil
}

func parseSeed(value string) ([ed25519.SeedSize]byte, bool) {
	var seed [ed25519.SeedSize]byte
	value = strings.TrimSpace(value)
	if len(value) == ed25519.SeedSize*2 {
		decoded, err := hex.DecodeString(value)
		if err == nil && len(decoded) == len(seed) {
			copy(seed[:], decoded)
			return seed, true
		}
	}
	if len(value) < ed25519.SeedSize {
		return seed, false
	}
	copy(seed[:], []byte(value)[:ed25519.SeedSize])
	return seed, true
}

func loadOrCreateSeed(dataDir string) ([ed25519.SeedSize]byte, error) {
	var seed [ed25519.SeedSize]byte
	if strings.TrimSpace(dataDir) == "" {
		return seed, errors.New("data directory is required when no signing seed is configured")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return seed, err
	}
	path := filepath.Join(dataDir, SigningSeedFile)
	readSeed := func() ([ed25519.SeedSize]byte, error) {
		var result [ed25519.SeedSize]byte
		info, err := os.Stat(path)
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return result, errors.New("signing key file must be regular and private")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return result, err
		}
		if len(data) != len(result) {
			return result, errors.New("signing key file must contain exactly 32 bytes")
		}
		copy(result[:], data)
		return result, nil
	}
	if existing, err := readSeed(); err == nil {
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return seed, err
	}
	if _, err := rand.Read(seed[:]); err != nil {
		return seed, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readSeed()
		}
		return [ed25519.SeedSize]byte{}, err
	}
	if _, err = file.Write(seed[:]); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return [ed25519.SeedSize]byte{}, err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return [ed25519.SeedSize]byte{}, err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
		return [ed25519.SeedSize]byte{}, err
	}
	return seed, nil
}

func (s *Surface) SignerPubkeyHex() string {
	if s == nil {
		return ""
	}
	return s.publicHex
}

func (s *Surface) UsingDevKey() bool { return s != nil && s.usingDevKey }

func (s *Surface) Recent() []FieldEvent {
	if s == nil {
		return []FieldEvent{}
	}
	s.mu.RLock()
	result := append([]FieldEvent(nil), s.recent...)
	s.mu.RUnlock()
	if result == nil {
		return []FieldEvent{}
	}
	return result
}

func (s *Surface) Snapshot() Response {
	return Response{Spec: "rufield", Endpoint: "/api/field", SignerPubkey: s.SignerPubkeyHex(), DevSigningKey: s.UsingDevKey(), Events: s.Recent()}
}

func (s *Surface) Subscribe() *stream.Subscription {
	if s == nil || s.events == nil {
		return nil
	}
	return s.events.Subscribe()
}

func (s *Surface) Close() {
	if s != nil && s.events != nil {
		s.events.Close()
	}
}

// Emit signs and publishes a single anonymous P2 event. The Go sensing state
// currently carries no governed trust class; this bridge therefore emits only
// the coarse occupancy/motion projection and never infers a lower-privacy
// class, personal identity, pose or numeric vital sign.
func (s *Surface) Emit(update state.SensingUpdate) (bool, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return false, errors.New("RuField surface is unavailable")
	}
	if !update.Classification.Presence {
		return false, nil
	}
	input, ok := snapshotFromUpdate(update)
	if !ok {
		return false, nil
	}
	event, err := snapshotToEvent(input, s.privateKey)
	if err != nil {
		return false, err
	}
	if err := VerifyEvent(event); err != nil {
		return false, fmt.Errorf("signed RuField event failed self-verification: %w", err)
	}
	encoded, err := marshalCompatible(event)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if len(s.recent) == RingCapacity {
		copy(s.recent, s.recent[1:])
		s.recent = s.recent[:RingCapacity-1]
	}
	s.recent = append(s.recent, event)
	s.mu.Unlock()
	s.events.Publish(encoded)
	return true, nil
}

type sensingSnapshot struct {
	timestampNS    uint64
	nodeID         string
	features       signal.FeatureInfo
	classification signal.Classification
	synthetic      bool
}

func snapshotFromUpdate(update state.SensingUpdate) (sensingSnapshot, bool) {
	timestampNS, timestampOK := timestampToNS(update.Timestamp)
	if !timestampOK {
		return sensingSnapshot{}, false
	}
	for _, value := range []float64{
		update.Features.MeanRSSI, update.Features.Variance, update.Features.MotionBandPower,
		update.Features.BreathingBandPower, update.Features.DominantFreqHz, update.Features.SpectralPower,
		update.Classification.Confidence,
	} {
		if !finite(value) {
			return sensingSnapshot{}, false
		}
	}
	nodeID := ""
	var newestAge uint64 = math.MaxUint64
	for _, node := range update.NodeFeatures {
		if node.LastSeenMS <= newestAge {
			newestAge = node.LastSeenMS
			nodeID = fmt.Sprintf("esp32_node_%d", node.NodeID)
		}
	}
	if nodeID == "" && len(update.Nodes) > 0 {
		nodeID = fmt.Sprintf("esp32_node_%d", update.Nodes[0].NodeID)
	}
	if nodeID == "" {
		return sensingSnapshot{}, false
	}
	return sensingSnapshot{
		timestampNS:    timestampNS,
		nodeID:         nodeID,
		features:       update.Features,
		classification: update.Classification,
		synthetic:      strings.Contains(strings.ToLower(update.Source), "simulat"),
	}, true
}

func timestampToNS(timestamp float64) (uint64, bool) {
	if !finite(timestamp) || timestamp <= 0 {
		return 0, false
	}
	seconds := math.Floor(timestamp)
	if seconds > float64(math.MaxUint64/1_000_000_000) {
		return 0, false
	}
	fraction := timestamp - seconds
	if fraction < 0 || fraction >= 1 {
		return 0, false
	}
	wholeNS := uint64(seconds) * 1_000_000_000
	fractionalNS := uint64(math.Round(fraction * 1_000_000_000))
	if fractionalNS >= 1_000_000_000 {
		if wholeNS > math.MaxUint64-1_000_000_000 {
			return 0, false
		}
		wholeNS += 1_000_000_000
		fractionalNS -= 1_000_000_000
	}
	if wholeNS > math.MaxUint64-fractionalNS {
		return 0, false
	}
	return wholeNS + fractionalNS, true
}

func snapshotToEvent(snapshot sensingSnapshot, privateKey ed25519.PrivateKey) (FieldEvent, error) {
	motion := normalizedMotionLevel(snapshot.classification.MotionLevel)
	confidence := float32(math.Max(0, math.Min(1, snapshot.classification.Confidence)))
	classificationFeatures := map[string]float32{
		"motion_energy":  squash(snapshot.features.MotionBandPower),
		"breathing_band": squash(snapshot.features.BreathingBandPower),
		"transient":      squash(float64(snapshot.features.ChangePoints)),
		"presence":       1,
	}
	measurementFeatures := snapshotFeatures(snapshot)
	tensor := FieldTensor{
		SpecVersion: SpecVersion, TimestampNS: snapshot.timestampNS, Modality: "wifi_csi",
		Axes: []string{"frequency"}, Shape: []int{len(measurementFeatures)}, Values: measurementFeatures,
		Confidence: confidence, NoiseFloor: max(float32(snapshot.features.Variance), 0), CalibrationID: nil, PrivacyClass: "P2",
	}
	tensorBytes, err := marshalCompatible(canonicalTensorFrom(tensor))
	if err != nil {
		return FieldEvent{}, err
	}
	labels := []string{"person_present", "motion_" + motion}
	observation := Observation{
		ZoneID: nil, SpaceCell: nil, RangeM: nil, VelocityMPS: nil,
		MotionVector: nil, Confidence: confidence, Features: classificationFeatures, Labels: labels,
		PrivacyClass: "P2",
	}
	firmwareHash := digestHex([]byte(ModelID))
	sensorVendor := "ruview"
	if strings.Contains(strings.ToLower(snapshot.nodeID), "esp32") {
		sensorVendor = "esp32"
	}
	provenance := ProvenanceRef{
		RawHash: digestHex(tensorBytes), FirmwareHash: firmwareHash, ModelID: ModelID,
		CalibrationID: "", Synthetic: snapshot.synthetic,
	}
	event := FieldEvent{
		SpecVersion: SpecVersion,
		EventID:     fmt.Sprintf("ruview-%s-%d", snapshot.nodeID, snapshot.timestampNS),
		TimestampNS: snapshot.timestampNS,
		Sensor:      SensorDescriptor{Modality: "wifi_csi", Vendor: sensorVendor, DeviceID: snapshot.nodeID, Placement: "unknown", ClockDomain: "local"},
		Tensor:      tensor, Observation: observation, Provenance: provenance,
	}
	canonical, err := canonicalEventBytes(event)
	if err != nil {
		return FieldEvent{}, err
	}
	signature := hex.EncodeToString(ed25519.Sign(privateKey, canonical))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	event.Provenance.SignatureHex = &signature
	publicHex := hex.EncodeToString(publicKey)
	event.Provenance.SignerPubkeyHex = &publicHex
	return event, nil
}

func snapshotFeatures(snapshot sensingSnapshot) []float32 {
	return []float32{
		float32(snapshot.features.MeanRSSI),
		float32(snapshot.features.Variance),
		float32(snapshot.features.MotionBandPower),
		float32(snapshot.features.BreathingBandPower),
		float32(snapshot.features.DominantFreqHz),
		float32(snapshot.features.SpectralPower),
	}
}

func squash(value float64) float32 {
	if !finite(value) || value <= 0 {
		return 0
	}
	return float32(value / (value + 1))
}

func normalizedMotionLevel(value string) string {
	switch value {
	case "active", "present_moving", "present_still", "absent":
		return value
	default:
		return "unknown"
	}
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func marshalCompatible(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	return append([]byte(nil), bytes.TrimSuffix(encoded, []byte("\n"))...), nil
}

func canonicalEventBytes(event FieldEvent) ([]byte, error) {
	event.Provenance.SignatureHex = nil
	event.Provenance.SignerPubkeyHex = nil
	return marshalCompatible(canonicalEvent{
		SpecVersion: event.SpecVersion, EventID: event.EventID, TimestampNS: event.TimestampNS,
		Sensor: canonicalSensor{Modality: event.Sensor.Modality, Vendor: event.Sensor.Vendor, DeviceID: event.Sensor.DeviceID, Placement: event.Sensor.Placement, CoordinateFrame: event.Sensor.CoordinateFrame, PositionM: event.Sensor.PositionM, OrientationXYZW: event.Sensor.OrientationXYZW, ClockDomain: event.Sensor.ClockDomain},
		Tensor: canonicalTensorFrom(event.Tensor), Observation: canonicalObservationFrom(event.Observation), Provenance: event.Provenance,
	})
}

type canonicalFloat32 float32

func (value canonicalFloat32) MarshalJSON() ([]byte, error) {
	encoded := strconv.FormatFloat(float64(value), 'g', -1, 32)
	if !strings.ContainsAny(encoded, ".eE") {
		encoded += ".0"
	}
	return []byte(encoded), nil
}

type canonicalEvent struct {
	SpecVersion string               `json:"spec_version"`
	EventID     string               `json:"event_id"`
	TimestampNS uint64               `json:"timestamp_ns"`
	Sensor      canonicalSensor      `json:"sensor"`
	Tensor      canonicalTensor      `json:"tensor"`
	Observation canonicalObservation `json:"observation"`
	Provenance  ProvenanceRef        `json:"provenance"`
}

type canonicalSensor struct {
	Modality        string      `json:"modality"`
	Vendor          string      `json:"vendor"`
	DeviceID        string      `json:"device_id"`
	Placement       string      `json:"placement"`
	CoordinateFrame *string     `json:"coordinate_frame,omitempty"`
	PositionM       *[3]float32 `json:"position_m,omitempty"`
	OrientationXYZW *[4]float32 `json:"orientation_xyzw,omitempty"`
	ClockDomain     string      `json:"clock_domain"`
}

type canonicalTensor struct {
	SpecVersion   string             `json:"spec_version"`
	TimestampNS   uint64             `json:"timestamp_ns"`
	Modality      string             `json:"modality"`
	Axes          []string           `json:"axes"`
	Shape         []int              `json:"shape"`
	Values        []canonicalFloat32 `json:"values"`
	Confidence    canonicalFloat32   `json:"confidence"`
	NoiseFloor    canonicalFloat32   `json:"noise_floor"`
	CalibrationID *string            `json:"calibration_id"`
	PrivacyClass  string             `json:"privacy_class"`
}

type canonicalObservation struct {
	ZoneID                    *string                     `json:"zone_id"`
	SpaceCell                 *[3]int32                   `json:"space_cell"`
	RangeM                    *canonicalFloat32           `json:"range_m"`
	VelocityMPS               *canonicalFloat32           `json:"velocity_mps"`
	MotionVector              *[3]canonicalFloat32        `json:"motion_vector"`
	Confidence                canonicalFloat32            `json:"confidence"`
	Features                  map[string]canonicalFloat32 `json:"features"`
	Labels                    []string                    `json:"labels"`
	PrivacyClass              string                      `json:"privacy_class"`
	TrackID                   *string                     `json:"track_id,omitempty"`
	Attributes                map[string]string           `json:"attributes,omitempty"`
	IdentityEvidence          json.RawMessage             `json:"identity_evidence,omitempty"`
	ChannelSoundingProvenance json.RawMessage             `json:"channel_sounding_provenance,omitempty"`
}

func canonicalTensorFrom(tensor FieldTensor) canonicalTensor {
	values := make([]canonicalFloat32, len(tensor.Values))
	for index, value := range tensor.Values {
		values[index] = canonicalFloat32(value)
	}
	return canonicalTensor{
		SpecVersion: tensor.SpecVersion, TimestampNS: tensor.TimestampNS, Modality: tensor.Modality,
		Axes: tensor.Axes, Shape: tensor.Shape, Values: values, Confidence: canonicalFloat32(tensor.Confidence),
		NoiseFloor: canonicalFloat32(tensor.NoiseFloor), CalibrationID: tensor.CalibrationID, PrivacyClass: tensor.PrivacyClass,
	}
}

func canonicalObservationFrom(observation Observation) canonicalObservation {
	features := make(map[string]canonicalFloat32, len(observation.Features))
	for key, value := range observation.Features {
		features[key] = canonicalFloat32(value)
	}
	var rangeM, velocity *canonicalFloat32
	if observation.RangeM != nil {
		value := canonicalFloat32(*observation.RangeM)
		rangeM = &value
	}
	if observation.VelocityMPS != nil {
		value := canonicalFloat32(*observation.VelocityMPS)
		velocity = &value
	}
	var motionVector *[3]canonicalFloat32
	if observation.MotionVector != nil {
		value := [3]canonicalFloat32{canonicalFloat32(observation.MotionVector[0]), canonicalFloat32(observation.MotionVector[1]), canonicalFloat32(observation.MotionVector[2])}
		motionVector = &value
	}
	return canonicalObservation{
		ZoneID: observation.ZoneID, SpaceCell: observation.SpaceCell, RangeM: rangeM, VelocityMPS: velocity,
		MotionVector: motionVector, Confidence: canonicalFloat32(observation.Confidence), Features: features,
		Labels: observation.Labels, PrivacyClass: observation.PrivacyClass, TrackID: observation.TrackID,
		Attributes: observation.Attributes, IdentityEvidence: observation.IdentityEvidence,
		ChannelSoundingProvenance: observation.ChannelSoundingProvenance,
	}
}

// VerifyEvent enforces the Go bridge's anonymous P2 projection and verifies
// RuField's detached Ed25519 signature, matching the canonical event contract.
// A valid signature proves integrity, not key enrollment or sensing accuracy.
func VerifyEvent(event FieldEvent) error {
	if event.SpecVersion != SpecVersion || event.TimestampNS == 0 || event.Tensor.TimestampNS != event.TimestampNS || event.Tensor.PrivacyClass != "P2" || event.Observation.PrivacyClass != "P2" {
		return ErrInvalidEvent
	}
	if event.Tensor.Modality != "wifi_csi" || len(event.Tensor.Axes) != 1 || event.Tensor.Axes[0] != "frequency" || len(event.Tensor.Shape) != 1 || event.Tensor.Shape[0] != 6 ||
		len(event.Tensor.Values) != 6 || event.Tensor.Confidence < 0 || event.Tensor.Confidence > 1 || event.Tensor.NoiseFloor < 0 || event.Tensor.CalibrationID != nil {
		return ErrInvalidEvent
	}
	for _, value := range event.Tensor.Values {
		if !finite(float64(value)) {
			return ErrInvalidEvent
		}
	}
	if event.Observation.ZoneID != nil || event.Observation.SpaceCell != nil || event.Observation.RangeM != nil || event.Observation.VelocityMPS != nil || event.Observation.MotionVector != nil ||
		event.Observation.TrackID != nil || event.Observation.IdentityEvidence != nil || event.Observation.ChannelSoundingProvenance != nil || event.Observation.Confidence != event.Tensor.Confidence || len(event.Observation.Attributes) != 0 {
		return ErrInvalidEvent
	}
	if len(event.Observation.Features) != 4 || event.Observation.Features["presence"] != 1 ||
		!validUnitFeature(event.Observation.Features["motion_energy"]) || !validUnitFeature(event.Observation.Features["breathing_band"]) || !validUnitFeature(event.Observation.Features["transient"]) ||
		len(event.Observation.Labels) != 2 || event.Observation.Labels[0] != "person_present" || !strings.HasPrefix(event.Observation.Labels[1], "motion_") || event.Provenance.CalibrationID != "" {
		return ErrInvalidEvent
	}
	for key, value := range event.Observation.Features {
		if (key != "presence" && key != "motion_energy" && key != "breathing_band" && key != "transient") || !validUnitFeature(value) {
			return ErrInvalidEvent
		}
	}
	rawHashBytes, err := marshalCompatible(canonicalTensorFrom(event.Tensor))
	if err != nil || event.Provenance.RawHash != digestHex(rawHashBytes) || event.Provenance.FirmwareHash != digestHex([]byte(ModelID)) || event.Provenance.ModelID != ModelID {
		return ErrInvalidEvent
	}
	if event.Provenance.SignatureHex == nil || event.Provenance.SignerPubkeyHex == nil {
		return ErrInvalidEvent
	}
	publicKey, err := hex.DecodeString(*event.Provenance.SignerPubkeyHex)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ErrInvalidEvent
	}
	signature, err := hex.DecodeString(*event.Provenance.SignatureHex)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrInvalidEvent
	}
	canonical, err := canonicalEventBytes(event)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), canonical, signature) {
		return ErrInvalidEvent
	}
	return nil
}

func validUnitFeature(value float32) bool { return finite(float64(value)) && value >= 0 && value <= 1 }
