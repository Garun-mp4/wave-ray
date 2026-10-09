package signal

import (
	"math"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

func TestDominantFrequencyFindsKnownTone(t *testing.T) {
	const sampleRate, target = 20.0, 1.25
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 12 + 2*math.Sin(2*math.Pi*target*float64(i)/sampleRate)
	}
	got := dominantFrequency(samples, sampleRate)
	if math.Abs(got-target) > 0.21 {
		t.Fatalf("got %.3f Hz, want %.2f Hz", got, target)
	}
}

func TestFieldIsBoundedAndHasExpectedGrid(t *testing.T) {
	field := generateSignalField(0.5, 0.25, 0.7, []float64{0.1, 2, 0.4, 1})
	if field.GridSize != [3]int{20, 1, 20} || len(field.Values) != 400 {
		t.Fatalf("bad field dimensions: %+v", field.GridSize)
	}
	for _, value := range field.Values {
		if value < 0 || value > 1 || math.IsNaN(value) {
			t.Fatalf("out-of-range cell: %v", value)
		}
	}
}

func TestProcessorKeepsBoundedHistoryAndProducesStableShape(t *testing.T) {
	p := NewProcessor()
	frame := protocol.CSIFrame{NodeID: 4, RSSIDBm: -50, NoiseFloor: -90, Subcarriers: 56, Amplitude: make([]float64, 56)}
	for i := 0; i < 120; i++ {
		for k := range frame.Amplitude {
			frame.Amplitude[k] = 20 + float64(k%7) + math.Sin(float64(i)*0.4)
		}
		obs := p.Push(frame, time.Now(), 20)
		if obs.NodeID != 4 || obs.Features.MeanRSSI != -50 || len(obs.Field.Values) != 400 {
			t.Fatalf("unexpected observation: %+v", obs)
		}
	}
	if len(p.frames) != FrameHistoryCapacity {
		t.Fatalf("history length=%d", len(p.frames))
	}
}

func TestProcessorResetsTemporalStateAfterLongGap(t *testing.T) {
	p := NewProcessor()
	frame := protocol.CSIFrame{
		NodeID: 4, RSSIDBm: -50, NoiseFloor: -90, Subcarriers: 4,
		Amplitude: []float64{20, 22, 19, 25}, Phase: []float64{0, 0.1, 0.2, 0.3},
	}
	start := time.Now()
	p.Push(frame, start, 20)
	p.Push(frame, start.Add(100*time.Millisecond), 20)
	if len(p.frames) != 2 || p.baselineFrames != 2 {
		t.Fatalf("processor did not build its initial window: frames=%d baseline=%d", len(p.frames), p.baselineFrames)
	}

	p.Push(frame, p.last.Add(historyResetAfter+time.Millisecond), 20)
	if len(p.frames) != 1 || len(p.meanHistory) != 1 || p.baselineFrames != 1 {
		t.Fatalf("stale temporal history survived source gap: frames=%d means=%d baseline=%d", len(p.frames), len(p.meanHistory), p.baselineFrames)
	}
}

func TestRawClassificationThresholds(t *testing.T) {
	for score, want := range map[float64]string{0: "absent", 0.05: "present_still", 0.13: "present_moving", 0.3: "active"} {
		if got := rawClassify(score); got != want {
			t.Errorf("rawClassify(%v)=%q, want %q", score, got, want)
		}
	}
}
