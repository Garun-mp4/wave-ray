package ingest

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/security"
)

type countingSink struct {
	csi          atomic.Uint64
	vitals       atomic.Uint64
	edgeVitals   atomic.Uint64
	features     atomic.Uint64
	sync         atomic.Uint64
	wasm         atomic.Uint64
	vendorCSI    atomic.Uint64
	realtekRadar atomic.Uint64
}

func (s *countingSink) IngestCSI(protocol.CSIFrame)                          { s.csi.Add(1) }
func (s *countingSink) IngestVitals(protocol.VitalsPacket)                   { s.vitals.Add(1) }
func (s *countingSink) IngestEdgeFusedVitals(protocol.EdgeFusedVitalsPacket) { s.edgeVitals.Add(1) }
func (s *countingSink) IngestFeatureState(protocol.FeatureStatePacket)       { s.features.Add(1) }
func (s *countingSink) IngestSync(protocol.SyncPacket)                       { s.sync.Add(1) }
func (s *countingSink) IngestWASMOutput(protocol.WASMOutputPacket)           { s.wasm.Add(1) }
func (s *countingSink) IngestVendorCSI(csivendor.Snapshot)                   { s.vendorCSI.Add(1) }
func (s *countingSink) IngestRadar(radar.Snapshot)                           { s.realtekRadar.Add(1) }

func TestListenUDPClassifiesMalformedUnknownAndValidPackets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allowlist, err := security.NewUDPAllowlist([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &countingSink{}
	receiver, err := ListenUDP(ctx, "127.0.0.1:0", allowlist, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = receiver.Close()
		receiver.Wait()
	})

	client, err := net.DialUDP("udp", nil, receiver.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("unknown")); err != nil {
		t.Fatal(err)
	}
	malformed := make([]byte, protocol.HeaderSize)
	binary.LittleEndian.PutUint32(malformed[:4], protocol.MagicCSI)
	if _, err := client.Write(malformed); err != nil {
		t.Fatal(err)
	}
	valid := make([]byte, protocol.HeaderSize+2)
	binary.LittleEndian.PutUint32(valid[:4], protocol.MagicCSI)
	valid[5] = 1
	binary.LittleEndian.PutUint16(valid[6:8], 1)
	if _, err := client.Write(valid); err != nil {
		t.Fatal(err)
	}
	syncPacket := make([]byte, protocol.SyncPacketSize)
	binary.LittleEndian.PutUint32(syncPacket[:4], protocol.MagicSync)
	syncPacket[5] = protocol.SyncProtocolVersion
	if _, err := client.Write(syncPacket); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(testMediaTekDatagram()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(testEdgeFusedVitalsDatagram()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(testWASMOutputDatagram()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(testRealtekRadarDatagram()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		counters := receiver.Counters()
		if counters.Datagrams == 8 {
			if counters.Unknown != 1 || counters.Malformed != 1 || counters.Accepted != 6 || sink.csi.Load() != 1 || sink.sync.Load() != 1 || sink.vendorCSI.Load() != 1 || sink.edgeVitals.Load() != 1 || sink.wasm.Load() != 1 || sink.realtekRadar.Load() != 1 {
				t.Fatalf("unexpected counters: %+v, sink CSI=%d sync=%d", counters, sink.csi.Load(), sink.sync.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("receiver did not process all test datagrams: %+v", receiver.Counters())
}

func testRealtekRadarDatagram() []byte {
	packet := make([]byte, 56+1+4)
	binary.LittleEndian.PutUint32(packet[0:4], radar.Magic)
	packet[4], packet[5] = radar.Version, 5 // version 1, capabilities report
	binary.LittleEndian.PutUint16(packet[6:8], 56)
	binary.LittleEndian.PutUint32(packet[8:12], uint32(len(packet)))
	binary.LittleEndian.PutUint32(packet[12:16], 7)
	binary.LittleEndian.PutUint64(packet[16:24], 9000)
	binary.LittleEndian.PutUint16(packet[36:38], 40)
	binary.LittleEndian.PutUint16(packet[40:42], 1)
	packet[42], packet[43] = 0, 1
	binary.LittleEndian.PutUint32(packet[44:48], 0x3f800000)
	packet[56] = 1
	binary.LittleEndian.PutUint32(packet[len(packet)-4:], crc32.ChecksumIEEE(packet[:len(packet)-4]))
	return packet
}

func testEdgeFusedVitalsDatagram() []byte {
	packet := make([]byte, protocol.EdgeFusedVitalsPacketSize)
	binary.LittleEndian.PutUint32(packet[:4], protocol.MagicEdgeFusedVitals)
	packet[4], packet[5], packet[12], packet[13] = 9, 0b00001001, 201, 1
	binary.LittleEndian.PutUint16(packet[6:8], 1600)
	binary.LittleEndian.PutUint32(packet[8:12], 720000)
	return packet
}

func testWASMOutputDatagram() []byte {
	packet := make([]byte, protocol.WASMOutputHeaderSize+protocol.WASMEventSize)
	binary.LittleEndian.PutUint32(packet[:4], protocol.MagicWASMOutput)
	packet[4], packet[5] = 3, 7
	binary.LittleEndian.PutUint16(packet[6:8], 1)
	packet[8] = 2
	binary.LittleEndian.PutUint32(packet[9:13], 0x3f800000)
	return packet
}

func testMediaTekDatagram() []byte {
	const headerLen = 72
	payload := []byte{211, 3, 0, 4, 0} // RSSI -45 dBm, I/Q = (3, 4)
	packet := make([]byte, headerLen+len(payload)+4)
	binary.LittleEndian.PutUint32(packet[0:4], csivendor.MagicMediaTek)
	packet[4], packet[5] = 1, 1
	binary.LittleEndian.PutUint16(packet[6:8], headerLen)
	binary.LittleEndian.PutUint32(packet[8:12], uint32(len(packet)))
	binary.LittleEndian.PutUint32(packet[12:16], 3)
	binary.LittleEndian.PutUint64(packet[16:24], 100)
	binary.LittleEndian.PutUint64(packet[24:32], 1)
	binary.LittleEndian.PutUint16(packet[32:34], 1)
	binary.LittleEndian.PutUint16(packet[34:36], 20)
	packet[42], packet[43], packet[44], packet[45] = 1, 1, 1, 1
	binary.LittleEndian.PutUint16(packet[46:48], 1)
	packet[48] = 1
	packet[49] = 166 // noise floor -90 dBm
	binary.LittleEndian.PutUint32(packet[52:56], 0x3f800000)
	binary.LittleEndian.PutUint32(packet[56:60], 0x48989680)
	binary.LittleEndian.PutUint32(packet[64:68], uint32(len(payload)))
	copy(packet[headerLen:], payload)
	binary.LittleEndian.PutUint32(packet[len(packet)-4:], crc32.ChecksumIEEE(packet[:len(packet)-4]))
	return packet
}

func TestListenUDPRequiresAllDependencies(t *testing.T) {
	ctx := context.Background()
	allowlist, _ := security.NewUDPAllowlist(nil)
	sink := &countingSink{}
	if _, err := ListenUDP(ctx, "127.0.0.1:0", allowlist, nil, nil); err == nil {
		t.Fatal("nil packet sink was accepted")
	}
	if _, err := ListenUDP(ctx, "127.0.0.1:0", nil, sink, nil); err == nil {
		t.Fatal("nil source allowlist was accepted")
	}
	if _, err := ListenUDP(nil, "127.0.0.1:0", allowlist, sink, nil); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func TestReceiverReportsCleanContextShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	allowlist, err := security.NewUDPAllowlist(nil)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := ListenUDP(ctx, "127.0.0.1:0", allowlist, &countingSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-receiver.Done():
	case <-time.After(time.Second):
		_ = receiver.Close()
		t.Fatal("UDP receiver did not stop when its context was canceled")
	}
	if err := receiver.Err(); err != nil {
		t.Fatalf("context cancellation was reported as an ingress failure: %v", err)
	}
}
