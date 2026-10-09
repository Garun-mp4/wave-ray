package radar

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"math"
	"testing"
)

func radarDatagram(report, format byte, count uint16, payload []byte) []byte {
	packet := make([]byte, HeaderSize+len(payload)+CRCSize)
	binary.LittleEndian.PutUint32(packet[0:4], Magic)
	packet[4], packet[5] = Version, report
	binary.LittleEndian.PutUint16(packet[6:8], HeaderSize)
	binary.LittleEndian.PutUint32(packet[8:12], uint32(len(packet)))
	binary.LittleEndian.PutUint32(packet[12:16], 42)
	binary.LittleEndian.PutUint64(packet[16:24], 123456)
	binary.LittleEndian.PutUint64(packet[24:32], 0x1122334455667788)
	binary.LittleEndian.PutUint32(packet[32:36], 2442000)
	binary.LittleEndian.PutUint16(packet[36:38], 40)
	binary.LittleEndian.PutUint16(packet[38:40], 1|1<<3|1<<15)
	binary.LittleEndian.PutUint16(packet[40:42], count)
	packet[42], packet[43] = format, 1
	binary.LittleEndian.PutUint32(packet[44:48], math.Float32bits(0.5))
	binary.LittleEndian.PutUint32(packet[48:52], math.Float32bits(0.25))
	binary.LittleEndian.PutUint32(packet[52:56], 0xaabbccdd)
	copy(packet[HeaderSize:], payload)
	binary.LittleEndian.PutUint32(packet[len(packet)-CRCSize:], crc32.ChecksumIEEE(packet[:len(packet)-CRCSize]))
	return packet
}

func TestParseRangeFrameSummarizesPeakAndProvenance(t *testing.T) {
	payload := make([]byte, 12)
	binary.LittleEndian.PutUint32(payload[0:4], math.Float32bits(3))
	binary.LittleEndian.PutUint32(payload[4:8], math.Float32bits(10))
	binary.LittleEndian.PutUint32(payload[8:12], math.Float32bits(4))
	snapshot, err := ParseDatagram(radarDatagram(byte(reportRangeNear), byte(formatPowerF32), 3, payload))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EventType != "realtek_radar" || snapshot.Source != "realtek:simulated" || snapshot.ReportType != "range_near" || snapshot.Sequence != 42 || snapshot.TimestampUS != 123456 || snapshot.DeviceID != "1122334455667788" {
		t.Fatalf("metadata mismatch: %+v", snapshot)
	}
	if snapshot.PeakRangeM == nil || *snapshot.PeakRangeM != 0.25 || snapshot.PeakPower == nil || *snapshot.PeakPower != 5 || snapshot.MeanCFRAmplitude != nil {
		t.Fatalf("peak summary mismatch: %+v", snapshot)
	}
	if !snapshot.Calibrated || !snapshot.TimeSynchronized || snapshot.CalibrationID != 0xaabbccdd || snapshot.BinSpacing != 0.25 {
		t.Fatalf("flags/calibration mismatch: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || string(encoded) == "" {
		t.Fatalf("snapshot must serialize without raw payload: %q, %v", encoded, err)
	}
	if string(encoded) == "" || containsJSONKey(encoded, "payload") {
		t.Fatalf("wire response unexpectedly exposes payload: %s", encoded)
	}
}

func TestParseCFRFrameOnlyPublishesMeanAmplitude(t *testing.T) {
	payload := make([]byte, 8)
	binary.LittleEndian.PutUint32(payload[0:4], math.Float32bits(3))
	binary.LittleEndian.PutUint32(payload[4:8], math.Float32bits(4))
	snapshot, err := ParseDatagram(radarDatagram(byte(reportCFR), byte(formatComplexF32), 1, payload))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MeanCFRAmplitude == nil || math.Abs(float64(*snapshot.MeanCFRAmplitude-2.5)) > 1e-6 {
		t.Fatalf("scaled CFR mean amplitude mismatch: %+v", snapshot)
	}
	if snapshot.PeakRangeM != nil || snapshot.PeakPower != nil {
		t.Fatalf("CFR payload must not be reported as range data: %+v", snapshot)
	}
}

func TestParseCapabilitiesAndRejectsMalformedFrames(t *testing.T) {
	capabilities := radarDatagram(byte(reportCapabilities), byte(formatBytes), 3, []byte{1, 1, 7})
	if _, err := ParseDatagram(capabilities); err != nil {
		t.Fatalf("valid capabilities frame rejected: %v", err)
	}
	tests := map[string]func([]byte) []byte{
		"truncated":      func(p []byte) []byte { return p[:len(p)-1] },
		"trailing bytes": func(p []byte) []byte { return append(p, 0) },
		"crc":            func(p []byte) []byte { p[len(p)-1] ^= 1; return p },
		"version":        func(p []byte) []byte { p[4]++; return p },
		"header length":  func(p []byte) []byte { binary.LittleEndian.PutUint16(p[6:8], 8); return p },
		"type format":    func(p []byte) []byte { p[42] = byte(formatPowerF32); return p },
		"bandwidth":      func(p []byte) []byte { binary.LittleEndian.PutUint16(p[36:38], 80); return p },
		"antenna count":  func(p []byte) []byte { p[43] = 0; return p },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			packet := append([]byte(nil), capabilities...)
			packet = mutate(packet)
			if _, err := ParseDatagram(packet); err == nil {
				t.Fatal("malformed frame was accepted")
			}
		})
	}
	rangeFrame := radarDatagram(byte(reportRangeNear), byte(formatPowerF32), 1, make([]byte, 4))
	binary.LittleEndian.PutUint32(rangeFrame[HeaderSize:HeaderSize+4], math.Float32bits(float32(math.NaN())))
	binary.LittleEndian.PutUint32(rangeFrame[len(rangeFrame)-4:], crc32.ChecksumIEEE(rangeFrame[:len(rangeFrame)-4]))
	if _, err := ParseDatagram(rangeFrame); err == nil {
		t.Fatal("non-finite power value was accepted")
	}
}

func TestScaledOverflowRejected(t *testing.T) {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, math.Float32bits(math.MaxFloat32))
	packet := radarDatagram(byte(reportRangeFar), byte(formatPowerF32), 1, payload)
	binary.LittleEndian.PutUint32(packet[44:48], math.Float32bits(math.MaxFloat32))
	binary.LittleEndian.PutUint32(packet[len(packet)-4:], crc32.ChecksumIEEE(packet[:len(packet)-4]))
	if _, err := ParseDatagram(packet); err == nil {
		t.Fatal("scaled non-finite output was accepted")
	}
}

func FuzzParseDatagram(f *testing.F) {
	f.Add(radarDatagram(byte(reportCapabilities), byte(formatBytes), 3, []byte{1, 1, 7}))
	f.Add([]byte("RTR1"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		if len(packet) > MaxFrameSize+1 {
			return
		}
		_, _ = ParseDatagram(packet)
	})
}

func containsJSONKey(data []byte, key string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	_, ok := object[key]
	return ok
}
