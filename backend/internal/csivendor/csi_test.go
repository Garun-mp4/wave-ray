package csivendor

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParseMediaTekAndQualcommMIMOCSI(t *testing.T) {
	mediatek := makeMIMO(MagicMediaTek, 1, 1, 1, 1, 20, 2, 1, 1<<15|1, []byte{wireByte(-45), 3, 0, 4, 0, 5, 0, 12, 0})
	snapshot, err := ParseDatagram(mediatek)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EventType != "mediatek_csi" || snapshot.Source != "mediatek:simulated" || !snapshot.Calibrated || !snapshot.Synthetic || snapshot.Chipset != "mt7981-mt7976" || snapshot.ElementCount != 2 || len(snapshot.RSSIDBm) != 1 || snapshot.RSSIDBm[0] != -45 {
		t.Fatalf("unexpected MediaTek snapshot: %+v", snapshot)
	}
	if snapshot.MeanAmplitude == nil || math.Abs(float64(*snapshot.MeanAmplitude-9)) > 0.001 || snapshot.PeakAmplitude == nil || math.Abs(float64(*snapshot.PeakAmplitude-13)) > 0.001 {
		t.Fatalf("unexpected amplitude summary: mean=%v peak=%v", snapshot.MeanAmplitude, snapshot.PeakAmplitude)
	}
	if snapshot.Frame == nil || snapshot.Frame.Sequence != 17 || snapshot.Frame.TimestampUS != 1_700_000_000_000_000 || len(snapshot.Frame.Chains) != 1 {
		t.Fatalf("MediaTek per-subcarrier CSI was not retained: %+v", snapshot.Frame)
	}
	chain := snapshot.Frame.Chains[0]
	if chain.TX != 0 || chain.RX != 0 || len(chain.Amplitude) != 2 || chain.Amplitude[0] != 5 || chain.Amplitude[1] != 13 || math.Abs(float64(chain.Phase[0])-math.Atan2(4, 3)) > 1e-6 {
		t.Fatalf("unexpected chain samples: %+v", chain)
	}
	qualcomm := makeMIMO(MagicQualcomm, 1, 1, 1, 1, 40, 1, 1, 0, []byte{wireByte(-50), 3, 0, 4, 0})
	snapshot, err = ParseDatagram(qualcomm)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EventType != "qualcomm_csi" || snapshot.Source != "qualcomm" || snapshot.Chipset != "qca9300" || snapshot.ElementCount != 1 {
		t.Fatalf("unexpected Qualcomm snapshot: %+v", snapshot)
	}
	if snapshot.Frame != nil {
		t.Fatal("non-MediaTek raw CSI was retained in the MediaTek-only inspection ring")
	}
}

func TestRingBoundsMemoryAndReturnsCopies(t *testing.T) {
	ring := &Ring{}
	chain := Chain{TX: 0, RX: 0, Amplitude: []float32{1, 2}, Phase: []float32{0, 1}}
	for i := uint32(0); i < MaxRingFrames+3; i++ {
		ring.Push(CSIFrame{Sequence: i, SubcarrierCount: 2, Chains: []Chain{chain}})
	}
	if ring.Len() != MaxRingFrames || ring.ByteSize() > MaxRingBytes {
		t.Fatalf("ring exceeded its configured bounds: frames=%d bytes=%d", ring.Len(), ring.ByteSize())
	}
	frames := ring.Tail(2)
	if len(frames) != 2 || frames[0].Sequence != MaxRingFrames+1 || frames[1].Sequence != MaxRingFrames+2 {
		t.Fatalf("ring tail was not chronological: %+v", frames)
	}
	frames[0].Chains[0].Amplitude[0] = 99
	if ring.Tail(2)[0].Chains[0].Amplitude[0] != 1 {
		t.Fatal("mutating a ring result changed stored frame data")
	}
	positions := ChainUnion(ring.Tail(1))
	if len(positions) != 1 || positions[0] != (ChainPosition{TX: 0, RX: 0}) {
		t.Fatalf("unexpected chain union: %+v", positions)
	}
}

func TestCapabilitiesReportsDoNotInventCSIStatistics(t *testing.T) {
	packet := makeMIMO(MagicQualcomm, 2, 1, 1, 1, 20, 0, 3, 0, []byte{1, 2, 3})
	snapshot, err := ParseDatagram(packet)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ReportKind != "capabilities" || snapshot.ElementCount != 3 || snapshot.MeanAmplitude != nil || snapshot.PeakAmplitude != nil || len(snapshot.RSSIDBm) != 0 {
		t.Fatalf("capabilities report included fabricated signal values: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"rssi_dbm":[]`) || !strings.Contains(string(encoded), `"mean_amplitude":null`) {
		t.Fatalf("capabilities response shape is not Rust-compatible: %s", encoded)
	}
}

func TestParseRealtekCSIAndSnapshotShape(t *testing.T) {
	packet := makeRealtek(1, 1, 16, []byte{3, 4, 5, 12})
	snapshot, err := ParseDatagram(packet)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EventType != "realtek_csi" || snapshot.Source != "realtek_csi:simulated" || snapshot.CSIMode != "active" || snapshot.ProtocolMode != "ofdm" || snapshot.PeerMAC != "01:02:03:04:05:06" || snapshot.BandwidthMHz != 40 || snapshot.ElementCount != 2 {
		t.Fatalf("unexpected Realtek snapshot: %+v", snapshot)
	}
	if snapshot.MeanAmplitude == nil || *snapshot.MeanAmplitude != 9 || snapshot.PeakAmplitude == nil || *snapshot.PeakAmplitude != 13 {
		t.Fatalf("unexpected Realtek amplitude summary: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"rssi_dbm":-52`) || strings.Contains(string(encoded), "rssi_dbm_single") {
		t.Fatalf("Realtek compatibility field name/type is wrong: %s", encoded)
	}
}

func TestInvalidVendorCSIFramesFailClosed(t *testing.T) {
	valid := makeMIMO(MagicMediaTek, 1, 1, 1, 1, 20, 1, 1, 0, []byte{wireByte(-40), 1, 0, 2, 0})
	badCRC := append([]byte(nil), valid...)
	badCRC[len(badCRC)-1] ^= 0x80
	short := make([]byte, 4)
	binary.LittleEndian.PutUint32(short, MagicMediaTek)
	for name, packet := range map[string][]byte{
		"short": short, "trailing": append(append([]byte(nil), valid...), 0), "crc": badCRC,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDatagram(packet); !errors.Is(err, ErrMalformed) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	qca160MHz := makeMIMO(MagicQualcomm, 1, 1, 1, 1, 160, 1, 1, 0, []byte{wireByte(-40), 1, 0, 2, 0})
	if _, err := ParseDatagram(qca160MHz); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unsupported QCA9300 bandwidth error=%v", err)
	}
	malformedRealtek := makeMIMO(MagicRealtek, 1, 1, 1, 1, 20, 1, 1, 0, nil)
	if _, err := ParseDatagram(malformedRealtek); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed Realtek frame error=%v", err)
	}
	if _, err := ParseDatagram([]byte{9, 0, 0, 0}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown packet magic error=%v", err)
	}
	badF32 := makeMIMO(MagicQualcomm, 1, 1, 1, 1, 20, 1, 2, 0, []byte{wireByte(-40), 0, 0, 0xc0, 0x7f, 0, 0, 0, 0})
	if _, err := ParseDatagram(badF32); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-finite CSI error=%v", err)
	}
	tooLargeAmplitude := makeMIMO(MagicMediaTek, 1, 1, 1, 1, 20, 1, 1, 0, []byte{wireByte(-40), 0xff, 0x7f, 0xff, 0x7f})
	binary.LittleEndian.PutUint32(tooLargeAmplitude[52:56], math.Float32bits(math.MaxFloat32))
	putCRC(tooLargeAmplitude)
	if _, err := ParseDatagram(tooLargeAmplitude); !errors.Is(err, ErrMalformed) {
		t.Fatalf("overflowing CSI amplitude error=%v", err)
	}
}

func FuzzParseDatagramNeverPanics(f *testing.F) {
	f.Add(makeMIMO(MagicMediaTek, 1, 1, 1, 1, 20, 1, 1, 0, []byte{wireByte(-40), 1, 0, 2, 0}))
	f.Add(makeRealtek(1, 0, 16, []byte{1, 2}))
	f.Add([]byte("not-a-frame"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		if len(packet) > MaxFrameLen {
			return
		}
		_, _ = ParseDatagram(packet)
	})
}

func makeMIMO(magic uint32, reportKind, tx, rx byte, chipset, bandwidth uint16, subcarriers uint16, format byte, flags uint16, payload []byte) []byte {
	frame := make([]byte, MIMOCSIHeader+len(payload)+CRCSize)
	binary.LittleEndian.PutUint32(frame[0:4], magic)
	frame[4], frame[5] = Version, reportKind
	binary.LittleEndian.PutUint16(frame[6:8], MIMOCSIHeader)
	binary.LittleEndian.PutUint32(frame[8:12], uint32(len(frame)))
	binary.LittleEndian.PutUint32(frame[12:16], 17)
	binary.LittleEndian.PutUint64(frame[16:24], 1_700_000_000_000_000)
	binary.LittleEndian.PutUint64(frame[24:32], 0x1234)
	binary.LittleEndian.PutUint16(frame[32:34], chipset)
	binary.LittleEndian.PutUint16(frame[34:36], bandwidth)
	binary.LittleEndian.PutUint32(frame[36:40], 2_412_000)
	binary.LittleEndian.PutUint16(frame[40:42], flags)
	frame[42], frame[43], frame[44], frame[45] = tx, rx, format, 1
	binary.LittleEndian.PutUint16(frame[46:48], subcarriers)
	if reportKind == 1 && (format == 1 || format == 2) {
		frame[48] = rx
	}
	frame[49] = wireByte(-92)
	binary.LittleEndian.PutUint32(frame[52:56], math.Float32bits(1))
	binary.LittleEndian.PutUint32(frame[56:60], math.Float32bits(312_500))
	binary.LittleEndian.PutUint32(frame[60:64], 9)
	binary.LittleEndian.PutUint32(frame[64:68], uint32(len(payload)))
	copy(frame[MIMOCSIHeader:], payload)
	putCRC(frame)
	return frame
}

func makeRealtek(mode, bandwidth, toneBits byte, payload []byte) []byte {
	frame := make([]byte, RealtekHeader+len(payload)+CRCSize)
	binary.LittleEndian.PutUint32(frame[0:4], MagicRealtek)
	frame[4] = Version
	binary.LittleEndian.PutUint16(frame[5:7], RealtekHeader)
	binary.LittleEndian.PutUint32(frame[7:11], uint32(len(frame)))
	frame[11], frame[12] = 7, mode
	binary.LittleEndian.PutUint32(frame[13:17], 99)
	binary.LittleEndian.PutUint32(frame[17:21], 12345)
	copy(frame[21:27], []byte{1, 2, 3, 4, 5, 6})
	copy(frame[27:33], []byte{6, 5, 4, 3, 2, 1})
	frame[33], frame[34], frame[35], frame[36] = 6, bandwidth, 54, 0
	count := uint16(len(payload) / (2 * (int(toneBits) / 16)))
	binary.LittleEndian.PutUint16(frame[37:39], count)
	frame[39], frame[40], frame[41], frame[42], frame[43], frame[44] = toneBits, 1, wireByte(-52), 3, 1, 1
	binary.LittleEndian.PutUint32(frame[45:49], uint32(len(payload)))
	copy(frame[RealtekHeader:], payload)
	putCRC(frame)
	return frame
}

func wireByte(value int8) byte { return byte(value) }
