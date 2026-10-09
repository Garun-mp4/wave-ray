package protocol

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"testing"
)

func TestParseEdgeFusedVitalsPacket(t *testing.T) {
	buf := make([]byte, EdgeFusedVitalsPacketSize)
	binary.LittleEndian.PutUint32(buf[:4], MagicEdgeFusedVitals)
	buf[4], buf[5], buf[12], buf[13], buf[14], buf[15] = 9, 0b00001001, 201, 1, 2, 85
	binary.LittleEndian.PutUint16(buf[6:8], 1600)
	binary.LittleEndian.PutUint32(buf[8:12], 720000)
	binary.LittleEndian.PutUint32(buf[16:20], math.Float32bits(.42))
	binary.LittleEndian.PutUint32(buf[20:24], math.Float32bits(.95))
	binary.LittleEndian.PutUint32(buf[24:28], 1234567)
	binary.LittleEndian.PutUint32(buf[28:32], math.Float32bits(71.5))
	binary.LittleEndian.PutUint32(buf[32:36], math.Float32bits(15.8))
	binary.LittleEndian.PutUint32(buf[36:40], math.Float32bits(182))
	buf[40], buf[41] = 1, 90
	packet, err := ParseEdgeFusedVitalsPacket(buf)
	if err != nil {
		t.Fatal(err)
	}
	if packet.NodeID != 9 || packet.BreathingRateBPM != 16 || packet.HeartRateBPM != 72 || packet.RSSIDBm != -55 || !packet.PersonCountValid || packet.Persons != 1 || packet.MmwaveType != 2 || packet.FusionConfidence != 85 || packet.TimestampMS != 1234567 || packet.MmwaveTargets != 1 || packet.MmwaveConfidence != 90 {
		t.Fatalf("unexpected packet: %+v", packet)
	}
	if packet.MotionEnergy < .419 || packet.MotionEnergy > .421 || packet.PresenceScore < .949 || packet.PresenceScore > .951 || packet.MmwaveHeartRateBPM != 71.5 || packet.MmwaveBreathingBPM < 15.79 || packet.MmwaveBreathingBPM > 15.81 || packet.MmwaveDistanceCM != 182 {
		t.Fatalf("unexpected float values: %+v", packet)
	}
	vitals := packet.Vitals()
	if !vitals.Presence || !vitals.CountValid || vitals.Persons != 1 || vitals.HeartRateBPM != 0 || vitals.BreathingBPM != 0 {
		t.Fatalf("conservative vitals projection incorrect: %+v", vitals)
	}
}

func TestParseEdgeFusedVitalsRejectsMalformedAndNonFinite(t *testing.T) {
	valid := make([]byte, EdgeFusedVitalsPacketSize)
	binary.LittleEndian.PutUint32(valid[:4], MagicEdgeFusedVitals)
	for _, malformed := range [][]byte{valid[:EdgeFusedVitalsPacketSize-1], append(append([]byte(nil), valid...), 0)} {
		if _, err := ParseEdgeFusedVitalsPacket(malformed); err == nil {
			t.Fatal("wrong packet length was accepted")
		}
	}
	bad := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint32(bad[28:32], math.Float32bits(float32(math.NaN())))
	if _, err := ParseEdgeFusedVitalsPacket(bad); err == nil {
		t.Fatal("non-finite radar telemetry was accepted")
	}
}

func TestParseWASMOutputPacketStrictlyDecodesEvents(t *testing.T) {
	buf := make([]byte, WASMOutputHeaderSize+2*WASMEventSize)
	binary.LittleEndian.PutUint32(buf[:4], MagicWASMOutput)
	buf[4], buf[5] = 3, 7
	binary.LittleEndian.PutUint16(buf[6:8], 2)
	buf[8] = 4
	binary.LittleEndian.PutUint32(buf[9:13], math.Float32bits(1.25))
	buf[13] = 8
	binary.LittleEndian.PutUint32(buf[14:18], math.Float32bits(-2.5))
	packet, err := ParseWASMOutputPacket(buf)
	if err != nil {
		t.Fatal(err)
	}
	if packet.NodeID != 3 || packet.ModuleID != 7 || len(packet.Events) != 2 || packet.Events[0] != (WASMEvent{EventType: 4, Value: 1.25}) || packet.Events[1] != (WASMEvent{EventType: 8, Value: -2.5}) {
		t.Fatalf("unexpected packet: %+v", packet)
	}
}

func TestParseWASMOutputRejectsCollisionTruncationTrailingAndNonFinite(t *testing.T) {
	fused := make([]byte, EdgeFusedVitalsPacketSize)
	binary.LittleEndian.PutUint32(fused[:4], MagicEdgeFusedVitals)
	if _, err := ParseWASMOutputPacket(fused); err == nil {
		t.Fatal("legacy fused-vitals magic was accepted as WASM")
	}
	for _, length := range []int{WASMOutputHeaderSize - 1, WASMOutputHeaderSize + WASMEventSize - 1, WASMOutputHeaderSize + WASMEventSize + 1} {
		buf := make([]byte, length)
		binary.LittleEndian.PutUint32(buf[:4], MagicWASMOutput)
		if length >= WASMOutputHeaderSize {
			binary.LittleEndian.PutUint16(buf[6:8], 1)
		}
		if _, err := ParseWASMOutputPacket(buf); err == nil {
			t.Fatalf("malformed WASM packet of %d bytes accepted", length)
		}
	}
	buf := make([]byte, WASMOutputHeaderSize+WASMEventSize)
	binary.LittleEndian.PutUint32(buf[:4], MagicWASMOutput)
	binary.LittleEndian.PutUint16(buf[6:8], 1)
	binary.LittleEndian.PutUint32(buf[9:13], math.Float32bits(float32(math.Inf(1))))
	if _, err := ParseWASMOutputPacket(buf); err == nil {
		t.Fatal("non-finite WASM event value was accepted")
	}
	tooMany := make([]byte, WASMOutputHeaderSize+17*WASMEventSize)
	binary.LittleEndian.PutUint32(tooMany[:4], MagicWASMOutput)
	binary.LittleEndian.PutUint16(tooMany[6:8], 17)
	if _, err := ParseWASMOutputPacket(tooMany); err == nil {
		t.Fatal("packet exceeding the firmware's 16-event limit was accepted")
	}
}

func TestParseCSIFrameADR018(t *testing.T) {
	buf := make([]byte, HeaderSize+4)
	binary.LittleEndian.PutUint32(buf[:4], MagicCSI)
	buf[4], buf[5] = 7, 1
	binary.LittleEndian.PutUint16(buf[6:8], 2)
	binary.LittleEndian.PutUint32(buf[8:12], 2412)
	binary.LittleEndian.PutUint32(buf[12:16], 42)
	buf[16], buf[17], buf[18] = 205, 161, 3
	buf[20], buf[21], buf[22], buf[23] = 3, 4, 0xff, 0
	frame, err := ParseCSIFrame(buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.NodeID != 7 || frame.Sequence != 42 || frame.RSSIDBm != -51 || frame.FrequencyMHz != 2412 {
		t.Fatalf("unexpected header: %+v", frame)
	}
	if math.Abs(frame.Amplitude[0]-5) > 1e-12 || math.Abs(frame.Phase[0]-math.Atan2(4, 3)) > 1e-12 {
		t.Fatalf("unexpected I/Q decode: %+v", frame)
	}
}

func TestParseCSIRejectsTruncationAndInvalidDimensions(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[:4], MagicCSI)
	buf[5] = 1
	binary.LittleEndian.PutUint16(buf[6:8], 1)
	if _, err := ParseCSIFrame(buf); err == nil {
		t.Fatal("truncated I/Q payload accepted")
	}
	buf = append(buf, 1, 2, 3)
	if _, err := ParseCSIFrame(buf); err != nil {
		t.Fatalf("legacy-compatible trailing extension was rejected: %v", err)
	}
	buf = make([]byte, HeaderSize+2)
	binary.LittleEndian.PutUint32(buf[:4], MagicCSI)
	buf[5] = 5
	binary.LittleEndian.PutUint16(buf[6:8], 1)
	if _, err := ParseCSIFrame(buf); err == nil {
		t.Fatal("antenna count above protocol bound accepted")
	}
}

func TestParseCSIAllowsSubcarrierCountsWithinFirmwarePairLimit(t *testing.T) {
	buf := make([]byte, HeaderSize+306*2)
	binary.LittleEndian.PutUint32(buf[:4], MagicCSI)
	buf[5] = 1
	binary.LittleEndian.PutUint16(buf[6:8], 306)
	if _, err := ParseCSIFrame(buf); err != nil {
		t.Fatalf("valid 306-subcarrier frame rejected: %v", err)
	}
}

func TestParseFeatureStateValidatesFirmwareCRCAndFields(t *testing.T) {
	buf := make([]byte, 60)
	binary.LittleEndian.PutUint32(buf[:4], MagicFeatureState)
	buf[4], buf[5] = 9, 2
	binary.LittleEndian.PutUint16(buf[6:8], 17)
	binary.LittleEndian.PutUint64(buf[8:16], 1234567)
	for offset, value := range map[int]float32{16: 0.7, 20: 0.9, 24: 15.5, 28: 0.8, 32: 70.25, 36: 0.6, 40: 0.2, 44: 0.1, 48: 0.95} {
		binary.LittleEndian.PutUint32(buf[offset:offset+4], math.Float32bits(value))
	}
	binary.LittleEndian.PutUint32(buf[56:60], crc32.ChecksumIEEE(buf[:56]))
	packet, err := ParseFeatureStatePacket(buf)
	if err != nil {
		t.Fatal(err)
	}
	if packet.NodeID != 9 || packet.Sequence != 17 || packet.TimestampUS != 1234567 || math.Abs(packet.HeartbeatBPM-70.25) > 1e-6 {
		t.Fatalf("unexpected feature state: %+v", packet)
	}
	buf[20] ^= 1
	if _, err := ParseFeatureStatePacket(buf); err == nil {
		t.Fatal("packet with invalid CRC was accepted")
	}
}

func TestParseVitalsFailsClosedOnContradictoryCount(t *testing.T) {
	buf := make([]byte, 32)
	binary.LittleEndian.PutUint32(buf[:4], MagicVitals)
	buf[4], buf[13] = 3, 7
	packet, err := ParseVitalsPacket(buf)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Persons != 0 || packet.CountValid {
		t.Fatalf("invalid count was not suppressed: %+v", packet)
	}
}

func TestParseVitalsRejectsNonFiniteValues(t *testing.T) {
	buf := make([]byte, 32)
	binary.LittleEndian.PutUint32(buf[:4], MagicVitals)
	binary.LittleEndian.PutUint32(buf[16:20], math.Float32bits(float32(math.NaN())))
	if _, err := ParseVitalsPacket(buf); err == nil {
		t.Fatal("NaN motion value accepted")
	}
}

func TestParseVitalsAllowsLegacyTrailingExtension(t *testing.T) {
	buf := make([]byte, 36)
	binary.LittleEndian.PutUint32(buf[:4], MagicVitals)
	if _, err := ParseVitalsPacket(buf); err != nil {
		t.Fatalf("legacy parser accepted packets with trailing extension bytes: %v", err)
	}
}
