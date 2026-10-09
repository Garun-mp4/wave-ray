package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	MagicCSI                  uint32 = 0xC5110001
	MagicVitals               uint32 = 0xC5110002
	MagicEdgeFusedVitals      uint32 = 0xC5110004
	MagicFeatureState         uint32 = 0xC5110006
	MagicWASMOutput           uint32 = 0xC5110007
	MagicSync                 uint32 = 0xC511A110
	HeaderSize                       = 20
	MaxAntennas                      = 4
	MaxIQPairs                       = 1024
	EdgeFusedVitalsPacketSize        = 48
	WASMOutputHeaderSize             = 8
	WASMEventSize                    = 5
	MaxWASMEvents                    = 16
	MaxUDPDatagramSize               = 65507
	// CSIFlagMeshSyncValid is ADR-018 byte 19 bit 4. It marks that the
	// mesh-clock synchronization was valid for this CSI frame.
	CSIFlagMeshSyncValid uint8 = 1 << 4
)

var (
	ErrMalformedPacket   = errors.New("malformed CSI packet")
	ErrUnsupportedPacket = errors.New("unsupported packet type")
)

// CSIFrame is the decoded ADR-018 ESP32 frame. Raw bytes are not retained.
type CSIFrame struct {
	// Source is process-local provenance. It is empty for the ESP32 wire format
	// and is never read from or written to a network packet.
	Source string
	// InputMode describes derived host measurements when Source is not a wire
	// packet source (for example, RSSI-derived pseudo-CSI).
	InputMode    string
	NodeID       uint8
	Antennas     uint8
	Subcarriers  uint16
	FrequencyMHz uint16
	Sequence     uint32
	RSSIDBm      int8
	NoiseFloor   int8
	PPDUType     uint8
	Flags        uint8
	Amplitude    []float64
	Phase        []float64
}

func ParseCSIFrame(buf []byte) (CSIFrame, error) {
	if len(buf) < HeaderSize || binary.LittleEndian.Uint32(buf[:4]) != MagicCSI {
		return CSIFrame{}, ErrMalformedPacket
	}
	antennas := buf[5]
	subcarriers := binary.LittleEndian.Uint16(buf[6:8])
	if antennas == 0 || antennas > MaxAntennas || subcarriers == 0 || int(antennas)*int(subcarriers) > MaxIQPairs {
		return CSIFrame{}, ErrMalformedPacket
	}
	pairs := int(antennas) * int(subcarriers)
	expected := HeaderSize + pairs*2
	if len(buf) < expected {
		return CSIFrame{}, ErrMalformedPacket
	}
	freq := binary.LittleEndian.Uint32(buf[8:12])
	if freq > math.MaxUint16 {
		freq = 0 // matches the legacy decoder's saturating conversion.
	}
	rssi := int8(buf[16])
	if rssi > 0 {
		rssi = -rssi
	}
	frame := CSIFrame{
		NodeID: buf[4], Antennas: antennas, Subcarriers: subcarriers,
		FrequencyMHz: uint16(freq), Sequence: binary.LittleEndian.Uint32(buf[12:16]),
		RSSIDBm: rssi, NoiseFloor: int8(buf[17]), PPDUType: buf[18], Flags: buf[19],
		Amplitude: make([]float64, pairs), Phase: make([]float64, pairs),
	}
	for i := 0; i < pairs; i++ {
		real, imag := float64(int8(buf[HeaderSize+i*2])), float64(int8(buf[HeaderSize+i*2+1]))
		frame.Amplitude[i] = math.Hypot(real, imag)
		frame.Phase[i] = math.Atan2(imag, real)
	}
	return frame, nil
}

// FeatureStatePacket is the 60-byte ADR-081 compact state frame emitted by
// current ESP32 firmware as its default, low-rate uplink.
type FeatureStatePacket struct {
	NodeID          uint8   `json:"node_id"`
	Mode            uint8   `json:"mode"`
	Sequence        uint16  `json:"sequence"`
	TimestampUS     uint64  `json:"timestamp_us"`
	MotionScore     float64 `json:"motion_score"`
	PresenceScore   float64 `json:"presence_score"`
	RespirationBPM  float64 `json:"respiration_bpm"`
	RespirationConf float64 `json:"respiration_conf"`
	HeartbeatBPM    float64 `json:"heartbeat_bpm"`
	HeartbeatConf   float64 `json:"heartbeat_conf"`
	AnomalyScore    float64 `json:"anomaly_score"`
	EnvShiftScore   float64 `json:"env_shift_score"`
	NodeCoherence   float64 `json:"node_coherence"`
	QualityFlags    uint16  `json:"quality_flags"`
	Reserved        uint16  `json:"-"`
}

func ParseFeatureStatePacket(buf []byte) (FeatureStatePacket, error) {
	if len(buf) != 60 || binary.LittleEndian.Uint32(buf[:4]) != MagicFeatureState {
		return FeatureStatePacket{}, ErrMalformedPacket
	}
	if got, want := crc32IEEE(buf[:56]), binary.LittleEndian.Uint32(buf[56:60]); got != want {
		return FeatureStatePacket{}, ErrMalformedPacket
	}
	f32 := func(offset int) float64 {
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[offset : offset+4])))
	}
	packet := FeatureStatePacket{
		NodeID: buf[4], Mode: buf[5], Sequence: binary.LittleEndian.Uint16(buf[6:8]),
		TimestampUS: binary.LittleEndian.Uint64(buf[8:16]), MotionScore: f32(16),
		PresenceScore: f32(20), RespirationBPM: f32(24), RespirationConf: f32(28),
		HeartbeatBPM: f32(32), HeartbeatConf: f32(36), AnomalyScore: f32(40),
		EnvShiftScore: f32(44), NodeCoherence: f32(48),
		QualityFlags: binary.LittleEndian.Uint16(buf[52:54]), Reserved: binary.LittleEndian.Uint16(buf[54:56]),
	}
	values := []float64{packet.MotionScore, packet.PresenceScore, packet.RespirationBPM, packet.RespirationConf, packet.HeartbeatBPM, packet.HeartbeatConf, packet.AnomalyScore, packet.EnvShiftScore, packet.NodeCoherence}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return FeatureStatePacket{}, ErrMalformedPacket
		}
	}
	if !unitInterval(packet.MotionScore) || !unitInterval(packet.PresenceScore) ||
		!unitInterval(packet.RespirationConf) || !unitInterval(packet.HeartbeatConf) ||
		!unitInterval(packet.AnomalyScore) || !unitInterval(packet.EnvShiftScore) || !unitInterval(packet.NodeCoherence) ||
		packet.RespirationBPM < 0 || packet.RespirationBPM > 120 || packet.HeartbeatBPM < 0 || packet.HeartbeatBPM > 300 {
		return FeatureStatePacket{}, ErrMalformedPacket
	}
	return packet, nil
}

func crc32IEEE(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, value := range data {
		crc ^= uint32(value)
		for bit := 0; bit < 8; bit++ {
			mask := uint32(0) - (crc & 1)
			crc = (crc >> 1) ^ (0xedb88320 & mask)
		}
	}
	return ^crc
}

func unitInterval(value float64) bool { return value >= 0 && value <= 1 }

type VitalsPacket struct {
	NodeID        uint8   `json:"node_id"`
	Presence      bool    `json:"presence"`
	FallDetected  bool    `json:"fall_detected"`
	Motion        bool    `json:"motion"`
	BreathingBPM  float64 `json:"breathing_rate_bpm"`
	HeartRateBPM  float64 `json:"heartrate_bpm"`
	RSSIDBm       int8    `json:"rssi_dbm"`
	Persons       uint8   `json:"n_persons"`
	CountValid    bool    `json:"person_count_valid"`
	MotionEnergy  float64 `json:"motion_energy"`
	PresenceScore float64 `json:"presence_score"`
	TimestampMS   uint32  `json:"timestamp_ms"`
}

// EdgeFusedVitalsPacket is the ADR-063 ESP32-C6/mmWave packed uplink.
// Sensor-provided biometrics remain telemetry; callers must apply the same
// trust and calibration gates as the Rust API before exposing them as results.
type EdgeFusedVitalsPacket struct {
	NodeID             uint8   `json:"node_id"`
	Flags              uint8   `json:"flags"`
	BreathingRateBPM   float64 `json:"breathing_rate_bpm"`
	HeartRateBPM       float64 `json:"heartrate_bpm"`
	RSSIDBm            int8    `json:"rssi"`
	Persons            uint8   `json:"n_persons"`
	PersonCountValid   bool    `json:"person_count_valid"`
	MmwaveType         uint8   `json:"mmwave_type"`
	FusionConfidence   uint8   `json:"fusion_confidence"`
	MotionEnergy       float64 `json:"motion_energy"`
	PresenceScore      float64 `json:"presence_score"`
	TimestampMS        uint32  `json:"timestamp_ms"`
	MmwaveHeartRateBPM float64 `json:"mmwave_hr_bpm"`
	MmwaveBreathingBPM float64 `json:"mmwave_br_bpm"`
	MmwaveDistanceCM   float64 `json:"mmwave_distance_cm"`
	MmwaveTargets      uint8   `json:"mmwave_targets"`
	MmwaveConfidence   uint8   `json:"mmwave_confidence"`
}

func ParseEdgeFusedVitalsPacket(buf []byte) (EdgeFusedVitalsPacket, error) {
	if len(buf) != EdgeFusedVitalsPacketSize || binary.LittleEndian.Uint32(buf[:4]) != MagicEdgeFusedVitals {
		return EdgeFusedVitalsPacket{}, ErrMalformedPacket
	}
	flags := buf[5]
	rawPersons := buf[13]
	presence := flags&0x09 != 0
	countValid := rawPersons <= 4 && (presence || rawPersons == 0)
	persons := rawPersons
	if !countValid {
		persons = 0
	}
	f32 := func(offset int) float64 {
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[offset : offset+4])))
	}
	packet := EdgeFusedVitalsPacket{
		NodeID: buf[4], Flags: flags,
		BreathingRateBPM: float64(binary.LittleEndian.Uint16(buf[6:8])) / 100,
		HeartRateBPM:     float64(binary.LittleEndian.Uint32(buf[8:12])) / 10000,
		RSSIDBm:          int8(buf[12]), Persons: persons, PersonCountValid: countValid,
		MmwaveType: buf[14], FusionConfidence: buf[15], MotionEnergy: f32(16),
		PresenceScore: f32(20), TimestampMS: binary.LittleEndian.Uint32(buf[24:28]),
		MmwaveHeartRateBPM: f32(28), MmwaveBreathingBPM: f32(32),
		MmwaveDistanceCM: f32(36), MmwaveTargets: buf[40], MmwaveConfidence: buf[41],
	}
	for _, value := range []float64{packet.MotionEnergy, packet.PresenceScore, packet.MmwaveHeartRateBPM, packet.MmwaveBreathingBPM, packet.MmwaveDistanceCM} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return EdgeFusedVitalsPacket{}, ErrMalformedPacket
		}
	}
	return packet, nil
}

// Vitals converts the fused packet to the conservative occupancy fields that
// are currently supported by the shared sensing state. It intentionally drops
// biometrics rather than publishing uncalibrated heart/breath rates there.
func (p EdgeFusedVitalsPacket) Vitals() VitalsPacket {
	return VitalsPacket{
		NodeID: p.NodeID, Presence: p.Flags&0x09 != 0,
		FallDetected: p.Flags&0x02 != 0, Motion: p.Flags&0x04 != 0,
		RSSIDBm: p.RSSIDBm, Persons: p.Persons, CountValid: p.PersonCountValid,
		MotionEnergy: p.MotionEnergy, PresenceScore: p.PresenceScore,
		TimestampMS: p.TimestampMS,
	}
}

type WASMEvent struct {
	EventType uint8   `json:"event_type"`
	Value     float32 `json:"value"`
}

type WASMOutputPacket struct {
	NodeID   uint8       `json:"node_id"`
	ModuleID uint8       `json:"module_id"`
	Events   []WASMEvent `json:"events"`
}

func ParseWASMOutputPacket(buf []byte) (WASMOutputPacket, error) {
	if len(buf) < WASMOutputHeaderSize || len(buf) > MaxUDPDatagramSize || binary.LittleEndian.Uint32(buf[:4]) != MagicWASMOutput {
		return WASMOutputPacket{}, ErrMalformedPacket
	}
	count := int(binary.LittleEndian.Uint16(buf[6:8]))
	if count > MaxWASMEvents {
		return WASMOutputPacket{}, ErrMalformedPacket
	}
	expected := WASMOutputHeaderSize + count*WASMEventSize
	if len(buf) != expected {
		return WASMOutputPacket{}, ErrMalformedPacket
	}
	packet := WASMOutputPacket{NodeID: buf[4], ModuleID: buf[5], Events: make([]WASMEvent, count)}
	for i := range packet.Events {
		offset := WASMOutputHeaderSize + i*WASMEventSize
		value := math.Float32frombits(binary.LittleEndian.Uint32(buf[offset+1 : offset+5]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return WASMOutputPacket{}, ErrMalformedPacket
		}
		packet.Events[i] = WASMEvent{EventType: buf[offset], Value: value}
	}
	return packet, nil
}

func ParseVitalsPacket(buf []byte) (VitalsPacket, error) {
	if len(buf) < 32 || binary.LittleEndian.Uint32(buf[:4]) != MagicVitals {
		return VitalsPacket{}, ErrMalformedPacket
	}
	flags := buf[5]
	rawCount := buf[13]
	present := flags&1 != 0
	validCount := rawCount <= 4 && (present || rawCount == 0)
	persons := rawCount
	if !validCount {
		persons = 0
	}
	motion := float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[16:20])))
	presenceScore := float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[20:24])))
	if math.IsNaN(motion) || math.IsInf(motion, 0) || math.IsNaN(presenceScore) || math.IsInf(presenceScore, 0) {
		return VitalsPacket{}, ErrMalformedPacket
	}
	return VitalsPacket{
		NodeID: buf[4], Presence: present, FallDetected: flags&2 != 0, Motion: flags&4 != 0,
		BreathingBPM: float64(binary.LittleEndian.Uint16(buf[6:8])) / 100,
		HeartRateBPM: float64(binary.LittleEndian.Uint32(buf[8:12])) / 10000,
		RSSIDBm:      int8(buf[12]), Persons: persons, CountValid: validCount,
		MotionEnergy: motion, PresenceScore: presenceScore,
		TimestampMS: binary.LittleEndian.Uint32(buf[24:28]),
	}, nil
}

// HasKnownMagic is a bounded dispatch helper. Unknown packets are rejected.
func HasKnownMagic(buf []byte) bool {
	if len(buf) < 4 {
		return false
	}
	switch binary.LittleEndian.Uint32(buf[:4]) {
	case MagicCSI, MagicVitals, MagicEdgeFusedVitals, MagicFeatureState, MagicWASMOutput, MagicSync:
		return true
	default:
		return false
	}
}
