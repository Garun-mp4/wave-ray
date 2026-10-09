package pose

import (
	"math"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

func TestWindowBufferEmitsOnlyAfterTwentyContiguousFrames(t *testing.T) {
	var buffer WindowBuffer
	base := time.Unix(500, 0)
	var output Window
	for sequence := uint32(1); sequence <= WindowFrames; sequence++ {
		var sample [Subcarriers]float32
		for tone := range sample {
			sample[tone] = float32(sequence*100 + uint32(tone))
		}
		window, ready := buffer.Push(sequence, base.Add(time.Duration(sequence)*50*time.Millisecond), sample)
		if ready != (sequence == WindowFrames) {
			t.Fatalf("sequence %d ready=%v", sequence, ready)
		}
		if ready {
			output = window
		}
	}
	if output[3][0] != 103 || output[3][WindowFrames-1] != float32(WindowFrames*100+3) {
		t.Fatalf("window is not ordered tone-major/oldest-first: %v", output[3])
	}
}

func TestWindowBufferResetsOnGapDuplicateAndInvalidSamples(t *testing.T) {
	base := time.Unix(600, 0)
	for _, tc := range []struct {
		name     string
		sequence uint32
		at       time.Time
		invalid  bool
	}{
		{name: "sequence gap", sequence: 3, at: base.Add(100 * time.Millisecond)},
		{name: "duplicate sequence", sequence: 1, at: base.Add(100 * time.Millisecond)},
		{name: "long frame gap", sequence: 2, at: base.Add(time.Second)},
		{name: "non finite", sequence: 2, at: base.Add(100 * time.Millisecond), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer WindowBuffer
			values := [Subcarriers]float32{}
			if _, ready := buffer.Push(1, base, values); ready {
				t.Fatal("one frame completed a temporal window")
			}
			if tc.invalid {
				values[0] = float32(math.NaN())
			}
			if _, ready := buffer.Push(tc.sequence, tc.at, values); ready {
				t.Fatal("discontinuous window was accepted")
			}
			if buffer.count != 1 && !tc.invalid {
				t.Fatalf("buffer was not reset before retaining new frame: count=%d", buffer.count)
			}
			if tc.invalid && buffer.count != 0 {
				t.Fatalf("invalid sample was retained: count=%d", buffer.count)
			}
		})
	}
}

func TestFeatureFromCSIRequiresRawOneAntenna56ToneInput(t *testing.T) {
	frame := protocol.CSIFrame{
		Source: "esp32", Antennas: 1, Subcarriers: Subcarriers,
		Amplitude: make([]float64, Subcarriers),
	}
	for i := range frame.Amplitude {
		frame.Amplitude[i] = float64(i) + 0.25
	}
	feature, err := FeatureFromCSI(frame)
	if err != nil || feature[3] != 3.25 {
		t.Fatalf("compatible raw CSI was not preserved: value=%v err=%v", feature[3], err)
	}
	frame.InputMode = "rssi_derived"
	if _, err := FeatureFromCSI(frame); err == nil {
		t.Fatal("RSSI-derived pseudo-CSI was accepted")
	}
	frame.InputMode, frame.Antennas = "", 2
	if _, err := FeatureFromCSI(frame); err == nil {
		t.Fatal("multi-antenna CSI was silently collapsed into the single-antenna training format")
	}
}
