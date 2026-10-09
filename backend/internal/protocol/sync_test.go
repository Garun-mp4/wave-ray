package protocol

import (
	"encoding/binary"
	"math"
	"testing"
)

func syncFixture() []byte {
	packet := make([]byte, SyncPacketSize)
	binary.LittleEndian.PutUint32(packet[0:4], MagicSync)
	packet[4] = 9
	packet[5] = SyncProtocolVersion
	packet[6] = 0x06
	binary.LittleEndian.PutUint64(packet[8:16], 28_798_450)
	binary.LittleEndian.PutUint64(packet[16:24], 27_634_885)
	binary.LittleEndian.PutUint32(packet[24:28], 20)
	return packet
}

func TestParseSyncPacketMatchesADR110Fixture(t *testing.T) {
	packet, err := ParseSyncPacket(syncFixture())
	if err != nil {
		t.Fatal(err)
	}
	if packet.NodeID != 9 || packet.Sequence != 20 || packet.OffsetUS() != 1_163_565 {
		t.Fatalf("unexpected sync packet: %+v offset=%d", packet, packet.OffsetUS())
	}
	if packet.Flags.IsLeader || !packet.Flags.IsValid || !packet.Flags.SmoothedUsed {
		t.Fatalf("sync flags were decoded incorrectly: %+v", packet.Flags)
	}
}

func TestParseSyncPacketRejectsBadSizeMagicAndVersion(t *testing.T) {
	fixture := syncFixture()
	for name, packet := range map[string][]byte{
		"short": fixture[:SyncPacketSize-1],
		"long":  append(append([]byte(nil), fixture...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSyncPacket(packet); err == nil {
				t.Fatal("invalid packet size was accepted")
			}
		})
	}
	badMagic := append([]byte(nil), fixture...)
	badMagic[0]++
	if _, err := ParseSyncPacket(badMagic); err == nil {
		t.Fatal("invalid magic was accepted")
	}
	badVersion := append([]byte(nil), fixture...)
	badVersion[5]++
	if _, err := ParseSyncPacket(badVersion); err == nil {
		t.Fatal("unsupported protocol version was accepted")
	}
	badFlags := append([]byte(nil), fixture...)
	badFlags[6] |= 0x80
	if _, err := ParseSyncPacket(badFlags); err == nil {
		t.Fatal("unknown flag bits were accepted")
	}
}

func TestSyncPacketEstimatesOnlyForwardMeshTime(t *testing.T) {
	packet, err := ParseSyncPacket(syncFixture())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := packet.MeshAlignedUSForSequence(22, 20)
	if !ok || got != packet.EpochUS+100_000 {
		t.Fatalf("mesh time=%d valid=%v, want %d", got, ok, packet.EpochUS+100_000)
	}
	if _, ok := packet.MeshAlignedUSForSequence(19, 20); ok {
		t.Fatal("older sequence was extrapolated into the future")
	}
	packet.Sequence = math.MaxUint32
	got, ok = packet.MeshAlignedUSForSequence(0, 20)
	if !ok || got != packet.EpochUS+50_000 {
		t.Fatalf("wrapped sequence mesh time=%d valid=%v", got, ok)
	}
	packet.Flags.IsValid = false
	if _, ok := packet.MeshAlignedUSForSequence(1, 20); ok {
		t.Fatal("invalid sync state was used for mesh-time recovery")
	}
}

func TestSyncPacketOffsetSaturatesOutOfRange(t *testing.T) {
	if got := (SyncPacket{LocalUS: math.MaxUint64, EpochUS: 0}).OffsetUS(); got != math.MaxInt64 {
		t.Fatalf("positive offset=%d, want saturation", got)
	}
	if got := (SyncPacket{LocalUS: 0, EpochUS: math.MaxUint64}).OffsetUS(); got != math.MinInt64 {
		t.Fatalf("negative offset=%d, want saturation", got)
	}
}

func FuzzParseSyncPacket(f *testing.F) {
	f.Add(syncFixture())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseSyncPacket(data)
	})
}
