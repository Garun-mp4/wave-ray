package ingest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/security"
)

type Sink interface {
	IngestCSI(protocol.CSIFrame)
	IngestVitals(protocol.VitalsPacket)
	IngestEdgeFusedVitals(protocol.EdgeFusedVitalsPacket)
	IngestFeatureState(protocol.FeatureStatePacket)
	IngestSync(protocol.SyncPacket)
	IngestWASMOutput(protocol.WASMOutputPacket)
	IngestRadar(radar.Snapshot)
}

type Counters struct {
	Datagrams uint64 `json:"datagrams"`
	Accepted  uint64 `json:"accepted"`
	Malformed uint64 `json:"malformed"`
	Denied    uint64 `json:"denied"`
	Unknown   uint64 `json:"unknown"`
}

type Receiver struct {
	conn      *net.UDPConn
	allowlist *security.UDPAllowlist
	sink      Sink
	logger    *slog.Logger
	done      chan struct{}
	errMu     sync.RWMutex
	err       error
	datagrams atomic.Uint64
	accepted  atomic.Uint64
	malformed atomic.Uint64
	denied    atomic.Uint64
	unknown   atomic.Uint64
}

func ListenUDP(ctx context.Context, addr string, allowlist *security.UDPAllowlist, sink Sink, logger *slog.Logger) (*Receiver, error) {
	if ctx == nil || sink == nil || allowlist == nil || addr == "" {
		return nil, errors.New("UDP receiver requires a context, address, allowlist, and packet sink")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve CSI UDP address: %w", err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for CSI UDP: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Receiver{conn: conn, allowlist: allowlist, sink: sink, logger: logger, done: make(chan struct{})}
	go r.run(ctx)
	return r, nil
}

func (r *Receiver) Addr() net.Addr        { return r.conn.LocalAddr() }
func (r *Receiver) Close() error          { return r.conn.Close() }
func (r *Receiver) Wait()                 { <-r.done }
func (r *Receiver) Done() <-chan struct{} { return r.done }

func (r *Receiver) Err() error {
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	return r.err
}

func (r *Receiver) setError(err error) {
	r.errMu.Lock()
	r.err = err
	r.errMu.Unlock()
}

func (r *Receiver) Counters() Counters {
	return Counters{Datagrams: r.datagrams.Load(), Accepted: r.accepted.Load(), Malformed: r.malformed.Load(), Denied: r.denied.Load(), Unknown: r.unknown.Load()}
}

func (r *Receiver) run(ctx context.Context) {
	defer close(r.done)
	defer r.conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = r.conn.Close() })
	defer stopClose()
	buffer := make([]byte, 65535)
	for {
		n, remote, err := r.conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			runErr := fmt.Errorf("read CSI UDP socket: %w", err)
			r.setError(runErr)
			r.logger.Error("CSI UDP receive failed", "error", runErr)
			return
		}
		r.datagrams.Add(1)
		ip, ok := netip.AddrFromSlice(remote.IP)
		if !ok || (r.allowlist != nil && !r.allowlist.AdmitAddr(ip.Unmap())) {
			r.denied.Add(1)
			continue
		}
		packet := buffer[:n]
		vendorCSIFrame := csivendor.HasKnownMagic(packet)
		radarFrame := radar.HasKnownMagic(packet)
		if !protocol.HasKnownMagic(packet) && !vendorCSIFrame && !radarFrame {
			r.unknown.Add(1)
			continue
		}
		if vendorCSIFrame {
			snapshot, parseErr := csivendor.ParseDatagram(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			sink, ok := r.sink.(interface{ IngestVendorCSI(csivendor.Snapshot) })
			if !ok {
				r.unknown.Add(1)
				continue
			}
			sink.IngestVendorCSI(snapshot)
			r.accepted.Add(1)
			continue
		}
		if radarFrame {
			snapshot, parseErr := radar.ParseDatagram(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestRadar(snapshot)
			r.accepted.Add(1)
			continue
		}
		magic := binary.LittleEndian.Uint32(packet[:4])
		switch magic {
		case protocol.MagicCSI:
			frame, parseErr := protocol.ParseCSIFrame(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestCSI(frame)
		case protocol.MagicVitals:
			vitals, parseErr := protocol.ParseVitalsPacket(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestVitals(vitals)
		case protocol.MagicEdgeFusedVitals:
			vitals, parseErr := protocol.ParseEdgeFusedVitalsPacket(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestEdgeFusedVitals(vitals)
		case protocol.MagicFeatureState:
			state, parseErr := protocol.ParseFeatureStatePacket(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestFeatureState(state)
		case protocol.MagicSync:
			syncPacket, parseErr := protocol.ParseSyncPacket(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestSync(syncPacket)
		case protocol.MagicWASMOutput:
			output, parseErr := protocol.ParseWASMOutputPacket(packet)
			if parseErr != nil {
				r.malformed.Add(1)
				continue
			}
			r.sink.IngestWASMOutput(output)
		default:
			r.unknown.Add(1)
			continue
		}
		r.accepted.Add(1)
	}
}
