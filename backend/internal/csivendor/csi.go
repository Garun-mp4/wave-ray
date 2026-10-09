// Package csivendor parses the versioned CSI bridge formats used by the
// MediaTek, Qualcomm, and Realtek transports. These are bridge contracts, not
// vendor firmware ABIs.
package csivendor

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	MagicMediaTek  uint32 = 0x3143544d // MTC1, little endian
	MagicQualcomm  uint32 = 0x31534351 // QCS1, little endian
	MagicRealtek   uint32 = 0x31434152 // RAC1, little endian
	Version               = 1
	MaxFrameLen           = 65_507
	MIMOCSIHeader         = 72
	RealtekHeader         = 49
	CRCSize               = 4
	MaxElements           = 16_384
	MaxSubcarriers        = 4_096
)

var (
	ErrMalformed   = errors.New("malformed vendor CSI frame")
	ErrUnsupported = errors.New("unsupported vendor CSI frame")
)

type Snapshot struct {
	EventType           string    `json:"event_type"`
	Source              string    `json:"source"`
	ReportKind          string    `json:"report_kind,omitempty"`
	Sequence            uint32    `json:"sequence"`
	TimestampUS         uint64    `json:"timestamp_us"`
	DeviceID            string    `json:"device_id,omitempty"`
	Chipset             string    `json:"chipset,omitempty"`
	CenterFreqKHz       uint32    `json:"center_freq_khz,omitempty"`
	BandwidthMHz        uint16    `json:"bandwidth_mhz"`
	TXCount             uint8     `json:"tx_count,omitempty"`
	RXCount             uint8     `json:"rx_count,omitempty"`
	SubcarrierCount     uint16    `json:"subcarrier_count,omitempty"`
	ElementCount        int       `json:"element_count"`
	PPDUType            string    `json:"ppdu_type,omitempty"`
	RSSIDBm             []int8    `json:"-"`
	NoiseFloorDBm       int8      `json:"noise_floor_dbm,omitempty"`
	Calibrated          bool      `json:"calibrated"`
	Synthetic           bool      `json:"synthetic"`
	Saturated           bool      `json:"saturated,omitempty"`
	TimeSynchronized    bool      `json:"time_synchronized,omitempty"`
	DroppedPredecessor  bool      `json:"dropped_predecessor,omitempty"`
	CalibrationID       uint32    `json:"calibration_id,omitempty"`
	SubcarrierSpacingHz float32   `json:"subcarrier_spacing_hz,omitempty"`
	MeanAmplitude       *float32  `json:"mean_amplitude"`
	PeakAmplitude       *float32  `json:"peak_amplitude"`
	CSIMode             string    `json:"csi_mode,omitempty"`
	NodeID              uint8     `json:"node_id,omitempty"`
	PeerMAC             string    `json:"peer_mac,omitempty"`
	TrigMAC             string    `json:"trig_mac,omitempty"`
	Channel             uint8     `json:"channel,omitempty"`
	RXRate              uint8     `json:"rx_rate,omitempty"`
	ProtocolMode        string    `json:"protocol_mode,omitempty"`
	NumSubcarrier       uint16    `json:"num_sub_carrier,omitempty"`
	NumBitPerTone       uint8     `json:"num_bit_per_tone,omitempty"`
	Decimation          uint8     `json:"decimation,omitempty"`
	RSSIDBmSingle       int8      `json:"-"`
	RXSC                uint8     `json:"rxsc,omitempty"`
	CSIValid            bool      `json:"csi_valid,omitempty"`
	Frame               *CSIFrame `json:"-"`
}

func (s Snapshot) MarshalJSON() ([]byte, error) {
	if s.EventType == "realtek_csi" {
		return json.Marshal(struct {
			EventType     string   `json:"event_type"`
			Source        string   `json:"source"`
			CSIMode       string   `json:"csi_mode"`
			NodeID        uint8    `json:"node_id"`
			Sequence      uint32   `json:"sequence"`
			TimestampUS   uint64   `json:"timestamp_us"`
			PeerMAC       string   `json:"peer_mac"`
			TrigMAC       string   `json:"trig_mac"`
			Channel       uint8    `json:"channel"`
			BandwidthMHz  uint16   `json:"bandwidth_mhz"`
			RXRate        uint8    `json:"rx_rate"`
			ProtocolMode  string   `json:"protocol_mode"`
			NumSubcarrier uint16   `json:"num_sub_carrier"`
			NumBitPerTone uint8    `json:"num_bit_per_tone"`
			Decimation    uint8    `json:"decimation"`
			RSSIDBm       int8     `json:"rssi_dbm"`
			RXSC          uint8    `json:"rxsc"`
			CSIValid      bool     `json:"csi_valid"`
			Synthetic     bool     `json:"synthetic"`
			MeanAmplitude *float32 `json:"mean_amplitude"`
			PeakAmplitude *float32 `json:"peak_amplitude"`
		}{s.EventType, s.Source, s.CSIMode, s.NodeID, s.Sequence, s.TimestampUS, s.PeerMAC, s.TrigMAC, s.Channel, s.BandwidthMHz, s.RXRate, s.ProtocolMode, s.NumSubcarrier, s.NumBitPerTone, s.Decimation, s.RSSIDBmSingle, s.RXSC, s.CSIValid, s.Synthetic, s.MeanAmplitude, s.PeakAmplitude})
	}
	return json.Marshal(struct {
		EventType           string   `json:"event_type"`
		Source              string   `json:"source"`
		ReportKind          string   `json:"report_kind"`
		Sequence            uint32   `json:"sequence"`
		TimestampUS         uint64   `json:"timestamp_us"`
		DeviceID            string   `json:"device_id"`
		Chipset             string   `json:"chipset"`
		CenterFreqKHz       uint32   `json:"center_freq_khz"`
		BandwidthMHz        uint16   `json:"bandwidth_mhz"`
		TXCount             uint8    `json:"tx_count"`
		RXCount             uint8    `json:"rx_count"`
		SubcarrierCount     uint16   `json:"subcarrier_count"`
		ElementCount        int      `json:"element_count"`
		PPDUType            string   `json:"ppdu_type"`
		RSSIDBm             []int8   `json:"rssi_dbm"`
		NoiseFloorDBm       int8     `json:"noise_floor_dbm"`
		Calibrated          bool     `json:"calibrated"`
		Synthetic           bool     `json:"synthetic"`
		Saturated           bool     `json:"saturated"`
		TimeSynchronized    bool     `json:"time_synchronized"`
		DroppedPredecessor  bool     `json:"dropped_predecessor"`
		CalibrationID       uint32   `json:"calibration_id"`
		SubcarrierSpacingHz float32  `json:"subcarrier_spacing_hz"`
		MeanAmplitude       *float32 `json:"mean_amplitude"`
		PeakAmplitude       *float32 `json:"peak_amplitude"`
	}{s.EventType, s.Source, s.ReportKind, s.Sequence, s.TimestampUS, s.DeviceID, s.Chipset, s.CenterFreqKHz, s.BandwidthMHz, s.TXCount, s.RXCount, s.SubcarrierCount, s.ElementCount, s.PPDUType, s.RSSIDBm, s.NoiseFloorDBm, s.Calibrated, s.Synthetic, s.Saturated, s.TimeSynchronized, s.DroppedPredecessor, s.CalibrationID, s.SubcarrierSpacingHz, s.MeanAmplitude, s.PeakAmplitude})
}

func HasKnownMagic(packet []byte) bool {
	return len(packet) >= 4 && isKnownMagic(binary.LittleEndian.Uint32(packet[:4]))
}

func isKnownMagic(magic uint32) bool {
	return magic == MagicMediaTek || magic == MagicQualcomm || magic == MagicRealtek
}

// ParseDatagram accepts exactly one complete frame. Extra bytes are rejected
// because each UDP datagram is the transport boundary for these formats.
func ParseDatagram(packet []byte) (Snapshot, error) {
	if len(packet) < 4 {
		return Snapshot{}, ErrMalformed
	}
	switch magic := binary.LittleEndian.Uint32(packet[:4]); magic {
	case MagicMediaTek:
		return parseMIMO(packet, true)
	case MagicQualcomm:
		return parseMIMO(packet, false)
	case MagicRealtek:
		return parseRealtek(packet)
	default:
		return Snapshot{}, ErrUnsupported
	}
}

func parseMIMO(packet []byte, mediatek bool) (Snapshot, error) {
	magic := MagicQualcomm
	if mediatek {
		magic = MagicMediaTek
	}
	if len(packet) < MIMOCSIHeader {
		return Snapshot{}, ErrMalformed
	}
	if len(packet) > MaxFrameLen || binary.LittleEndian.Uint32(packet[:4]) != magic || packet[4] != Version || binary.LittleEndian.Uint16(packet[6:8]) != MIMOCSIHeader {
		return Snapshot{}, ErrMalformed
	}
	frameLen := uint64(binary.LittleEndian.Uint32(packet[8:12]))
	if frameLen < MIMOCSIHeader+CRCSize || frameLen > MaxFrameLen || frameLen != uint64(len(packet)) {
		return Snapshot{}, ErrMalformed
	}
	if crc32.ChecksumIEEE(packet[:len(packet)-CRCSize]) != binary.LittleEndian.Uint32(packet[len(packet)-CRCSize:]) {
		return Snapshot{}, fmt.Errorf("%w: CRC mismatch", ErrMalformed)
	}

	reportKind, format, ppdu := packet[5], packet[44], packet[45]
	if reportKind != 1 && reportKind != 2 {
		return Snapshot{}, ErrMalformed
	}
	chipsetID := binary.LittleEndian.Uint16(packet[32:34])
	chipset, maxChains, maxBandwidth, ok := mimoChipset(chipsetID, mediatek)
	if !ok {
		return Snapshot{}, ErrMalformed
	}
	ppduName, ok := ppduName(ppdu, mediatek)
	if !ok {
		return Snapshot{}, ErrMalformed
	}
	bandwidth := binary.LittleEndian.Uint16(packet[34:36])
	if bandwidth != 20 && bandwidth != 40 && bandwidth != 80 && bandwidth != 160 {
		return Snapshot{}, ErrMalformed
	}
	if !mediatek && bandwidth > maxBandwidth {
		return Snapshot{}, ErrMalformed
	}
	txCount, rxCount := packet[42], packet[43]
	if txCount == 0 || rxCount == 0 || txCount > maxChains || rxCount > maxChains {
		return Snapshot{}, ErrMalformed
	}
	subcarriers := binary.LittleEndian.Uint16(packet[46:48])
	elements64 := uint64(txCount) * uint64(rxCount) * uint64(subcarriers)
	elements := int(elements64)
	rssiCount := int(packet[48])
	payloadLen := uint64(binary.LittleEndian.Uint32(packet[64:68]))
	if payloadLen+MIMOCSIHeader+CRCSize != frameLen {
		return Snapshot{}, ErrMalformed
	}
	payload := packet[MIMOCSIHeader : len(packet)-CRCSize]

	scale := math.Float32frombits(binary.LittleEndian.Uint32(packet[52:56]))
	spacing := math.Float32frombits(binary.LittleEndian.Uint32(packet[56:60]))
	if !finite32(scale) || scale <= 0 || !finite32(spacing) || spacing <= 0 {
		return Snapshot{}, ErrMalformed
	}
	flags := binary.LittleEndian.Uint16(packet[40:42])
	synthetic := flags&(1<<15) != 0
	calibrated := flags&1 != 0
	source := "qualcomm"
	eventType := "qualcomm_csi"
	if synthetic {
		source = "qualcomm:simulated"
	}
	if mediatek {
		eventType = "mediatek_csi"
		source = "mediatek:physical-unvalidated"
		if calibrated {
			source = "mediatek"
		}
		if synthetic {
			source = "mediatek:simulated"
		}
	}

	snapshot := Snapshot{
		EventType: eventType, Source: source, ReportKind: map[byte]string{1: "csi", 2: "capabilities"}[reportKind],
		Sequence: binary.LittleEndian.Uint32(packet[12:16]), TimestampUS: binary.LittleEndian.Uint64(packet[16:24]),
		DeviceID: fmt.Sprintf("%016x", binary.LittleEndian.Uint64(packet[24:32])), Chipset: chipset,
		BandwidthMHz: bandwidth, CenterFreqKHz: binary.LittleEndian.Uint32(packet[36:40]),
		TXCount: txCount, RXCount: rxCount, SubcarrierCount: subcarriers, ElementCount: 0,
		PPDUType: ppduName, RSSIDBm: []int8{}, NoiseFloorDBm: int8(packet[49]), Calibrated: calibrated,
		Synthetic: synthetic, Saturated: flags&(1<<1) != 0, TimeSynchronized: flags&(1<<2) != 0,
		DroppedPredecessor: flags&(1<<3) != 0, CalibrationID: binary.LittleEndian.Uint32(packet[60:64]), SubcarrierSpacingHz: spacing,
	}

	if format == 3 { // bytes, used for capabilities reports only
		if reportKind != 2 || len(payload) == 0 {
			return Snapshot{}, ErrMalformed
		}
		snapshot.ElementCount = len(payload)
		return snapshot, nil
	}
	if format != 1 && format != 2 || reportKind != 1 {
		return Snapshot{}, ErrMalformed
	}
	if elements64 == 0 || elements64 > MaxElements {
		return Snapshot{}, ErrMalformed
	}
	componentBytes := 4
	if format == 2 {
		componentBytes = 8
	}
	if rssiCount > len(payload) || len(payload)-rssiCount != elements*componentBytes || rssiCount != int(rxCount) {
		return Snapshot{}, ErrMalformed
	}
	snapshot.RSSIDBm = make([]int8, rssiCount)
	for i := range snapshot.RSSIDBm {
		snapshot.RSSIDBm[i] = int8(payload[i])
	}
	amplitudes := make([]float32, 0, elements)
	var iqValues [][2]float32
	if mediatek {
		iqValues = make([][2]float32, 0, elements)
	}
	valueData := payload[rssiCount:]
	if format == 1 {
		for i := 0; i < len(valueData); i += 4 {
			real := float32(int16(binary.LittleEndian.Uint16(valueData[i : i+2])))
			imag := float32(int16(binary.LittleEndian.Uint16(valueData[i+2 : i+4])))
			amplitude := float32(math.Hypot(float64(real), float64(imag))) * scale
			if !finite32(amplitude) {
				return Snapshot{}, ErrMalformed
			}
			amplitudes = append(amplitudes, amplitude)
			if mediatek {
				iqValues = append(iqValues, [2]float32{real, imag})
			}
		}
	} else {
		for i := 0; i < len(valueData); i += 8 {
			real := math.Float32frombits(binary.LittleEndian.Uint32(valueData[i : i+4]))
			imag := math.Float32frombits(binary.LittleEndian.Uint32(valueData[i+4 : i+8]))
			if !finite32(real) || !finite32(imag) {
				return Snapshot{}, ErrMalformed
			}
			amplitude := float32(math.Hypot(float64(real), float64(imag))) * scale
			if !finite32(amplitude) {
				return Snapshot{}, ErrMalformed
			}
			amplitudes = append(amplitudes, amplitude)
			if mediatek {
				iqValues = append(iqValues, [2]float32{real, imag})
			}
		}
	}
	snapshot.ElementCount = len(amplitudes)
	snapshot.MeanAmplitude, snapshot.PeakAmplitude = summarize(amplitudes)
	if mediatek {
		snapshot.Frame = frameFromIQ(snapshot, iqValues, scale)
	}
	return snapshot, nil
}

func parseRealtek(packet []byte) (Snapshot, error) {
	if len(packet) < RealtekHeader || len(packet) > MaxFrameLen || binary.LittleEndian.Uint32(packet[:4]) != MagicRealtek || packet[4] != Version || binary.LittleEndian.Uint16(packet[5:7]) != RealtekHeader {
		return Snapshot{}, ErrMalformed
	}
	frameLen := uint64(binary.LittleEndian.Uint32(packet[7:11]))
	if frameLen < RealtekHeader+CRCSize || frameLen > MaxFrameLen || frameLen != uint64(len(packet)) {
		return Snapshot{}, ErrMalformed
	}
	if crc32.ChecksumIEEE(packet[:len(packet)-CRCSize]) != binary.LittleEndian.Uint32(packet[len(packet)-CRCSize:]) {
		return Snapshot{}, fmt.Errorf("%w: CRC mismatch", ErrMalformed)
	}
	mode := packet[12]
	modeName, ok := map[byte]string{0: "passive", 1: "active", 2: "board_to_board"}[mode]
	if !ok {
		return Snapshot{}, ErrMalformed
	}
	protocol := packet[36]
	protocolName, ok := map[byte]string{0: "ofdm", 1: "ht", 2: "vht", 3: "he"}[protocol]
	if !ok {
		return Snapshot{}, ErrMalformed
	}
	bandwidth, toneBits := packet[34], packet[39]
	if bandwidth > 1 {
		return Snapshot{}, ErrMalformed
	}
	componentBytes := 0
	switch toneBits {
	case 16:
		componentBytes = 1
	case 32:
		componentBytes = 2
	default:
		return Snapshot{}, ErrMalformed
	}
	subcarriers := binary.LittleEndian.Uint16(packet[37:39])
	if subcarriers == 0 || int(subcarriers) > MaxSubcarriers {
		return Snapshot{}, ErrMalformed
	}
	payloadLen := uint64(binary.LittleEndian.Uint32(packet[45:49]))
	if payloadLen+RealtekHeader+CRCSize != frameLen || payloadLen != uint64(subcarriers)*uint64(componentBytes*2) {
		return Snapshot{}, ErrMalformed
	}
	payload := packet[RealtekHeader : len(packet)-CRCSize]
	amplitudes := make([]float32, 0, subcarriers)
	for i := 0; i < len(payload); i += componentBytes * 2 {
		var real, imag int16
		if componentBytes == 1 {
			real = int16(int8(payload[i]))
			imag = int16(int8(payload[i+1]))
		} else {
			real = int16(binary.LittleEndian.Uint16(payload[i : i+2]))
			imag = int16(binary.LittleEndian.Uint16(payload[i+2 : i+4]))
		}
		amplitudes = append(amplitudes, float32(math.Hypot(float64(real), float64(imag))))
	}
	mean, peak := summarize(amplitudes)
	flags := packet[44]
	synthetic := flags&1 != 0
	source := "realtek_csi"
	if synthetic {
		source = "realtek_csi:simulated"
	}
	return Snapshot{
		EventType: "realtek_csi", Source: source, CSIMode: modeName, NodeID: packet[11], Sequence: binary.LittleEndian.Uint32(packet[13:17]),
		TimestampUS: uint64(binary.LittleEndian.Uint32(packet[17:21])), PeerMAC: formatMAC(packet[21:27]), TrigMAC: formatMAC(packet[27:33]),
		Channel: packet[33], BandwidthMHz: map[byte]uint16{0: 20, 1: 40}[bandwidth], RXRate: packet[35], ProtocolMode: protocolName,
		NumSubcarrier: subcarriers, NumBitPerTone: toneBits, Decimation: packet[40], RSSIDBmSingle: int8(packet[41]),
		RXSC: packet[42], CSIValid: packet[43] != 0, Synthetic: synthetic, ElementCount: int(subcarriers), MeanAmplitude: mean, PeakAmplitude: peak,
	}, nil
}

func mimoChipset(id uint16, mediatek bool) (name string, maxChains uint8, maxBandwidth uint16, ok bool) {
	if mediatek {
		switch id {
		case 1:
			return "mt7981-mt7976", 3, 160, true
		case 2:
			return "mt7986-mt7975", 4, 160, true
		case 3:
			return "mt7988-mt7996", 4, 160, true
		}
	} else {
		switch id {
		case 1:
			return "qca9300", 3, 40, true
		case 2:
			return "qcn9074", 4, 160, true
		case 3:
			return "qcn9274", 4, 160, true
		}
	}
	return "", 0, 0, false
}

func ppduName(value byte, mediatek bool) (string, bool) {
	names := map[byte]string{1: "ht", 2: "vht", 3: "hesu", 4: "hemu", 5: "eht"}
	if mediatek {
		names[6] = "legacy"
		names[7] = "unknown"
	}
	name, ok := names[value]
	return name, ok
}

func summarize(values []float32) (*float32, *float32) {
	if len(values) == 0 {
		return nil, nil
	}
	var sum float32
	peak := values[0]
	for _, value := range values {
		sum += value
		if value > peak {
			peak = value
		}
	}
	mean := sum / float32(len(values))
	if !finite32(mean) || !finite32(peak) {
		return nil, nil
	}
	return &mean, &peak
}

func formatMAC(value []byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", value[0], value[1], value[2], value[3], value[4], value[5])
}
func finite32(value float32) bool   { return !float32NaN(value) && !float32Inf(value) }
func float32NaN(value float32) bool { return math.IsNaN(float64(value)) }
func float32Inf(value float32) bool { return math.IsInf(float64(value), 0) }

func putCRC(packet []byte) {
	binary.LittleEndian.PutUint32(packet[len(packet)-CRCSize:], crc32.ChecksumIEEE(packet[:len(packet)-CRCSize]))
}
