package csivendor

import (
	"testing"
	"time"
)

func TestFastActivityUsesTimestampWindowAndChannelVariation(t *testing.T) {
	ring := &Ring{}
	for i, amplitude := range []float32{2, 4, 8} {
		ring.Push(CSIFrame{
			TimestampUS: uint64(i) * 1_000_000, Sequence: uint32(i), SubcarrierCount: 1,
			Chains: []Chain{{TX: 0, RX: 0, Amplitude: []float32{amplitude}, Phase: []float32{0}}},
		})
	}
	fast, frames := ring.FastActivity(time.Second)
	if frames != 2 || fast <= 0 {
		t.Fatalf("last two frames should produce nonzero coefficient of variation: fast=%f frames=%d", fast, frames)
	}
	fast, frames = ring.FastActivity(500 * time.Millisecond)
	if frames != 1 || fast != 0 {
		t.Fatalf("short window should contain only newest frame: fast=%f frames=%d", fast, frames)
	}
}

func TestFastActivityFlatSeriesIsZero(t *testing.T) {
	ring := &Ring{}
	for i := 0; i < 4; i++ {
		ring.Push(CSIFrame{
			TimestampUS: uint64(i) * 250_000, SubcarrierCount: 2,
			Chains: []Chain{{TX: 0, RX: 0, Amplitude: []float32{3, 7}, Phase: []float32{0, 0}}},
		})
	}
	fast, frames := ring.FastActivity(2 * time.Second)
	if frames != 4 || fast != 0 {
		t.Fatalf("flat signal should have zero variation: fast=%f frames=%d", fast, frames)
	}
}
