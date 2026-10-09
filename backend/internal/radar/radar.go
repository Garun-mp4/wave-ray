// Package radar decodes the versioned RTL8720F bridge transport and exposes
// aggregate metadata. It intentionally does not turn radar bins into people,
// tracks, or localized coordinates.
package radar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

const (
	Magic        uint32 = 0x31525452 // RTR1, little endian
	Version             = 1
	HeaderSize          = 56
	CRCSize             = 4
	MaxFrameSize        = 65_507
	MaxElements         = 16_384
)

var (
	ErrMalformed   = errors.New("malformed RTL8720F radar frame")
	ErrUnsupported = errors.New("unsupported RTL8720F radar frame")
)

type reportType uint8

const (
	reportCFR reportType = iota + 1
	reportRangeNear
	reportRangeFar
	reportInterference
	reportCapabilities
)

type elementFormat uint8

const (
	formatBytes elementFormat = iota
	formatComplexI16
	formatComplexF32
	formatPowerU16
	formatPowerF32
)

type powerValue struct{ value float32 }
type complexValue struct{ i, q float32 }

// Snapshot is the privacy-conscious, payload-free latest view of one frame.
type Snapshot struct {
	EventType            string   `json:"event_type"`
	Source               string   `json:"source"`
	ReportType           string   `json:"report_type"`
	Sequence             uint32   `json:"sequence"`
	TimestampUS          uint64   `json:"timestamp_us"`
	DeviceID             string   `json:"device_id"`
	CenterFreqKHz        uint32   `json:"center_freq_khz"`
	BandwidthMHz         uint16   `json:"bandwidth_mhz"`
	AntennaCount         uint8    `json:"antenna_count"`
	ElementCount         int      `json:"element_count"`
	Calibrated           bool     `json:"calibrated"`
	Synthetic            bool     `json:"synthetic"`
	InterferenceDetected bool     `json:"interference_detected"`
	Saturated            bool     `json:"saturated"`
	TimeSynchronized     bool     `json:"time_synchronized"`
	CalibrationID        uint32   `json:"calibration_id"`
	BinSpacing           float32  `json:"bin_spacing"`
	PeakRangeM           *float32 `json:"peak_range_m"`
	PeakPower            *float32 `json:"peak_power"`
	MeanCFRAmplitude     *float32 `json:"mean_cfr_amplitude"`
}

type frame struct {
	report        reportType
	format        elementFormat
	sequence      uint32
	timestampUS   uint64
	deviceID      uint64
	centerFreqKHz uint32
	bandwidthMHz  uint16
	flags         uint16
	elementCount  int
	antennaCount  uint8
	scale         float32
	binSpacing    float32
	calibrationID uint32
	powers        []powerValue
	complex       []complexValue
}

func HasKnownMagic(packet []byte) bool {
	return len(packet) >= 4 && binary.LittleEndian.Uint32(packet[:4]) == Magic
}

// ParseDatagram accepts one complete frame per UDP datagram. Appended or
// truncated bytes are rejected rather than silently ignored.
func ParseDatagram(packet []byte) (Snapshot, error) {
	if len(packet) < HeaderSize+CRCSize || len(packet) > MaxFrameSize {
		return Snapshot{}, ErrMalformed
	}
	if binary.LittleEndian.Uint32(packet[:4]) != Magic {
		return Snapshot{}, ErrUnsupported
	}
	if packet[4] != Version {
		return Snapshot{}, fmt.Errorf("%w: version %d", ErrUnsupported, packet[4])
	}
	report := reportType(packet[5])
	headerLen := int(binary.LittleEndian.Uint16(packet[6:8]))
	frameLen := int(binary.LittleEndian.Uint32(packet[8:12]))
	if headerLen < HeaderSize || frameLen != len(packet) || frameLen < headerLen+CRCSize || frameLen > MaxFrameSize {
		return Snapshot{}, ErrMalformed
	}
	elementCount := int(binary.LittleEndian.Uint16(packet[40:42]))
	if elementCount > MaxElements {
		return Snapshot{}, ErrMalformed
	}
	format := elementFormat(packet[42])
	if !validTypeFormat(report, format) {
		return Snapshot{}, ErrMalformed
	}
	bytesPerElement, ok := elementByteSize(format)
	if !ok || elementCount > MaxFrameSize/bytesPerElement {
		return Snapshot{}, ErrMalformed
	}
	payloadLen := elementCount * bytesPerElement
	if headerLen+payloadLen+CRCSize != frameLen {
		return Snapshot{}, ErrMalformed
	}
	if crc32.ChecksumIEEE(packet[:frameLen-CRCSize]) != binary.LittleEndian.Uint32(packet[frameLen-CRCSize:]) {
		return Snapshot{}, fmt.Errorf("%w: CRC mismatch", ErrMalformed)
	}
	bandwidth := binary.LittleEndian.Uint16(packet[36:38])
	antennaCount := packet[43]
	scale := math.Float32frombits(binary.LittleEndian.Uint32(packet[44:48]))
	binSpacing := math.Float32frombits(binary.LittleEndian.Uint32(packet[48:52]))
	if (bandwidth != 20 && bandwidth != 40 && bandwidth != 70) || antennaCount == 0 || antennaCount > 8 || !finite(scale) || !finite(binSpacing) {
		return Snapshot{}, ErrMalformed
	}
	f := frame{
		report: report, format: format,
		sequence:      binary.LittleEndian.Uint32(packet[12:16]),
		timestampUS:   binary.LittleEndian.Uint64(packet[16:24]),
		deviceID:      binary.LittleEndian.Uint64(packet[24:32]),
		centerFreqKHz: binary.LittleEndian.Uint32(packet[32:36]),
		bandwidthMHz:  bandwidth, flags: binary.LittleEndian.Uint16(packet[38:40]),
		elementCount: elementCount, antennaCount: antennaCount, scale: scale, binSpacing: binSpacing,
		calibrationID: binary.LittleEndian.Uint32(packet[52:56]),
	}
	data := packet[headerLen : headerLen+payloadLen]
	switch format {
	case formatPowerU16:
		f.powers = make([]powerValue, elementCount)
		for i := range f.powers {
			f.powers[i].value = float32(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
		}
	case formatPowerF32:
		f.powers = make([]powerValue, elementCount)
		for i := range f.powers {
			value := math.Float32frombits(binary.LittleEndian.Uint32(data[i*4 : i*4+4]))
			if !finite(value) || !finite(value*scale) {
				return Snapshot{}, ErrMalformed
			}
			f.powers[i].value = value
		}
	case formatComplexI16:
		f.complex = make([]complexValue, elementCount)
		for i := range f.complex {
			f.complex[i] = complexValue{
				i: float32(int16(binary.LittleEndian.Uint16(data[i*4 : i*4+2]))),
				q: float32(int16(binary.LittleEndian.Uint16(data[i*4+2 : i*4+4]))),
			}
		}
	case formatComplexF32:
		f.complex = make([]complexValue, elementCount)
		for i := range f.complex {
			iVal := math.Float32frombits(binary.LittleEndian.Uint32(data[i*8 : i*8+4]))
			qVal := math.Float32frombits(binary.LittleEndian.Uint32(data[i*8+4 : i*8+8]))
			if !finite(iVal) || !finite(qVal) || !finite(float32(math.Hypot(float64(iVal), float64(qVal)))*scale) {
				return Snapshot{}, ErrMalformed
			}
			f.complex[i] = complexValue{i: iVal, q: qVal}
		}
	}
	return summarize(f)
}

func summarize(f frame) (Snapshot, error) {
	name, ok := reportName(f.report)
	if !ok {
		return Snapshot{}, ErrMalformed
	}
	synthetic := f.flags&(1<<15) != 0
	source := "realtek"
	if synthetic {
		source = "realtek:simulated"
	}
	s := Snapshot{
		EventType: "realtek_radar", Source: source, ReportType: name,
		Sequence: f.sequence, TimestampUS: f.timestampUS,
		DeviceID: fmt.Sprintf("%016x", f.deviceID), CenterFreqKHz: f.centerFreqKHz,
		BandwidthMHz: f.bandwidthMHz, AntennaCount: f.antennaCount, ElementCount: f.elementCount,
		Calibrated: f.flags&1 != 0, Synthetic: synthetic, InterferenceDetected: f.flags&(1<<1) != 0,
		Saturated: f.flags&(1<<2) != 0, TimeSynchronized: f.flags&(1<<3) != 0,
		CalibrationID: f.calibrationID,
		BinSpacing:    f.binSpacing,
	}
	// Keep scalar aggregation behavior aligned with the Rust summary adapter.
	if f.report == reportRangeNear || f.report == reportRangeFar {
		if len(f.powers) > 0 {
			peakIndex := 0
			peak := scaledPower(f.powers[0].value, f.format, f.scale)
			for i := 1; i < len(f.powers); i++ {
				value := scaledPower(f.powers[i].value, f.format, f.scale)
				if value > peak {
					peakIndex, peak = i, value
				}
			}
			rangeM := float32(peakIndex) * f.binSpacing
			if !finite(rangeM) || !finite(peak) {
				return Snapshot{}, ErrMalformed
			}
			s.PeakRangeM, s.PeakPower = &rangeM, &peak
		}
	}
	if f.report == reportCFR && len(f.complex) > 0 {
		var total float64
		for _, value := range f.complex {
			amp := math.Hypot(float64(value.i), float64(value.q)) * float64(f.scale)
			if math.IsNaN(amp) || math.IsInf(amp, 0) {
				return Snapshot{}, ErrMalformed
			}
			total += amp
		}
		mean := float32(total / float64(len(f.complex)))
		if !finite(mean) {
			return Snapshot{}, ErrMalformed
		}
		s.MeanCFRAmplitude = &mean
	}
	return s, nil
}

func scaledPower(value float32, format elementFormat, scale float32) float32 {
	if format == formatPowerU16 || format == formatPowerF32 {
		return value * scale
	}
	return 0
}

func reportName(t reportType) (string, bool) {
	switch t {
	case reportCFR:
		return "cfr", true
	case reportRangeNear:
		return "range_near", true
	case reportRangeFar:
		return "range_far", true
	case reportInterference:
		return "interference", true
	case reportCapabilities:
		return "capabilities", true
	default:
		return "", false
	}
}

func elementByteSize(f elementFormat) (int, bool) {
	switch f {
	case formatBytes:
		return 1, true
	case formatComplexI16, formatPowerF32:
		return 4, true
	case formatComplexF32:
		return 8, true
	case formatPowerU16:
		return 2, true
	default:
		return 0, false
	}
}

func validTypeFormat(report reportType, format elementFormat) bool {
	switch report {
	case reportCFR:
		return format == formatComplexI16 || format == formatComplexF32
	case reportRangeNear, reportRangeFar:
		return format == formatPowerU16 || format == formatPowerF32
	case reportInterference, reportCapabilities:
		return format == formatBytes
	default:
		return false
	}
}

func finite(v float32) bool { return !float32NaN(v) && !float32Inf(v) }

func float32NaN(v float32) bool { return math.IsNaN(float64(v)) }

func float32Inf(v float32) bool { return math.IsInf(float64(v), 0) }
