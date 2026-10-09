package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	SyncPacketSize       = 32
	SyncProtocolVersion  = 1
	syncFlagLeader       = 1 << 0
	syncFlagValid        = 1 << 1
	syncFlagSmoothedUsed = 1 << 2
)

var ErrMalformedSyncPacket = errors.New("malformed mesh sync packet")

// SyncFlags are the status bits emitted by the ESP32 mesh clock synchronizer.
type SyncFlags struct {
	IsLeader     bool `json:"is_leader"`
	IsValid      bool `json:"is_valid"`
	SmoothedUsed bool `json:"smoothed_used"`
}

// SyncPacket is the ADR-110 32-byte mesh clock synchronization datagram.
// LocalUS and EpochUS are ESP timer values in microseconds.
type SyncPacket struct {
	NodeID   uint8     `json:"node_id"`
	ProtoVer uint8     `json:"proto_ver"`
	Flags    SyncFlags `json:"flags"`
	LocalUS  uint64    `json:"local_us"`
	EpochUS  uint64    `json:"epoch_us"`
	Sequence uint32    `json:"sequence"`
}

func ParseSyncPacket(buf []byte) (SyncPacket, error) {
	if len(buf) != SyncPacketSize || binary.LittleEndian.Uint32(buf[:4]) != MagicSync {
		return SyncPacket{}, ErrMalformedSyncPacket
	}
	if buf[5] != SyncProtocolVersion || buf[6]&^byte(syncFlagLeader|syncFlagValid|syncFlagSmoothedUsed) != 0 {
		return SyncPacket{}, ErrUnsupportedPacket
	}
	flags := buf[6]
	return SyncPacket{
		NodeID: buf[4], ProtoVer: buf[5],
		Flags: SyncFlags{
			IsLeader: flags&syncFlagLeader != 0, IsValid: flags&syncFlagValid != 0,
			SmoothedUsed: flags&syncFlagSmoothedUsed != 0,
		},
		LocalUS: binary.LittleEndian.Uint64(buf[8:16]), EpochUS: binary.LittleEndian.Uint64(buf[16:24]),
		Sequence: binary.LittleEndian.Uint32(buf[24:28]),
	}, nil
}

// OffsetUS returns local minus mesh epoch, saturating malformed extreme wire
// values instead of overflowing the signed API representation.
func (p SyncPacket) OffsetUS() int64 {
	if p.LocalUS >= p.EpochUS {
		delta := p.LocalUS - p.EpochUS
		if delta > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(delta)
	}
	delta := p.EpochUS - p.LocalUS
	if delta >= uint64(math.MaxInt64)+1 {
		return math.MinInt64
	}
	return -int64(delta)
}

// MeshAlignedUSForSequence estimates a CSI frame's mesh epoch from the
// packet's high-water sequence and a measured frame rate. It only extrapolates
// forward (including uint32 wrap), matching the Rust sensing server.
func (p SyncPacket) MeshAlignedUSForSequence(frameSequence uint32, fpsHz float64) (uint64, bool) {
	if !p.Flags.IsValid || math.IsNaN(fpsHz) || math.IsInf(fpsHz, 0) || fpsHz < 1 || fpsHz > 50 {
		return 0, false
	}
	deltaFrames := frameSequence - p.Sequence
	if deltaFrames > math.MaxInt32 {
		return 0, false
	}
	deltaUS := int64(float64(deltaFrames) * 1_000_000 / fpsHz)
	if deltaUS >= 0 {
		increment := uint64(deltaUS)
		if p.EpochUS > math.MaxUint64-increment {
			return 0, false
		}
		return p.EpochUS + increment, true
	}
	decrement := uint64(-deltaUS)
	if p.EpochUS < decrement {
		return 0, false
	}
	return p.EpochUS - decrement, true
}
