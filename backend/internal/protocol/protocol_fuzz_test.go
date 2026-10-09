package protocol

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func FuzzParseCSIFrame(f *testing.F) {
	valid := make([]byte, HeaderSize+2)
	binary.LittleEndian.PutUint32(valid[:4], MagicCSI)
	valid[5] = 1
	binary.LittleEndian.PutUint16(valid[6:8], 1)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, 65535))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseCSIFrame(data)
	})
}

func FuzzParseVitalsPacket(f *testing.F) {
	valid := make([]byte, 32)
	binary.LittleEndian.PutUint32(valid[:4], MagicVitals)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, 65535))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseVitalsPacket(data)
	})
}

func FuzzParseEdgeFusedVitalsPacket(f *testing.F) {
	valid := make([]byte, EdgeFusedVitalsPacketSize)
	binary.LittleEndian.PutUint32(valid[:4], MagicEdgeFusedVitals)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, 65535))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseEdgeFusedVitalsPacket(data)
	})
}

func FuzzParseWASMOutputPacket(f *testing.F) {
	valid := make([]byte, WASMOutputHeaderSize)
	binary.LittleEndian.PutUint32(valid[:4], MagicWASMOutput)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, 65535))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseWASMOutputPacket(data)
	})
}

func FuzzParseFeatureStatePacket(f *testing.F) {
	valid := make([]byte, 60)
	binary.LittleEndian.PutUint32(valid[:4], MagicFeatureState)
	binary.LittleEndian.PutUint32(valid[56:60], crc32.ChecksumIEEE(valid[:56]))
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, 65535))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseFeatureStatePacket(data)
	})
}
