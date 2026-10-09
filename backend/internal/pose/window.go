package pose

import (
	"errors"
	"math"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

const maxWindowGap = 750 * time.Millisecond

type bufferedFrame struct {
	sequence uint32
	at       time.Time
	values   [Subcarriers]float32
}

// WindowBuffer keeps only the latest 20 amplitude frames in memory. A sequence
// gap or long pause discards the partial window so discontinuous samples are
// never presented to the temporal model as a contiguous recording.
type WindowBuffer struct {
	frames [WindowFrames]bufferedFrame
	count  int
	next   int
}

func FeatureFromCSI(frame protocol.CSIFrame) ([Subcarriers]float32, error) {
	var feature [Subcarriers]float32
	if frame.Source != "esp32" && frame.Source != "simulate" && frame.Source != "synthetic" {
		return feature, errors.New("pose inference accepts only raw ESP32 CSI or explicit simulation")
	}
	if frame.InputMode == "rssi_derived" {
		return feature, errors.New("RSSI-derived pseudo-CSI is not a pose input")
	}
	if frame.Antennas != 1 || frame.Subcarriers != Subcarriers || len(frame.Amplitude) != Subcarriers {
		return feature, errors.New("pose_v1 requires exactly one 56-subcarrier CSI amplitude vector")
	}
	for i, value := range frame.Amplitude {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e6 {
			return [Subcarriers]float32{}, errors.New("CSI amplitude contains an invalid value")
		}
		feature[i] = float32(value)
	}
	return feature, nil
}

func (b *WindowBuffer) Push(sequence uint32, at time.Time, values [Subcarriers]float32) (Window, bool) {
	if b == nil || at.IsZero() {
		return Window{}, false
	}
	for _, value := range values {
		if !finite32(value) {
			b.Reset()
			return Window{}, false
		}
	}
	if b.count > 0 {
		last := b.frames[(b.next-1+WindowFrames)%WindowFrames]
		if sequence != last.sequence+1 || !at.After(last.at) || at.Sub(last.at) > maxWindowGap {
			b.Reset()
		}
	}
	b.frames[b.next] = bufferedFrame{sequence: sequence, at: at, values: values}
	b.next = (b.next + 1) % WindowFrames
	if b.count < WindowFrames {
		b.count++
	}
	if b.count < WindowFrames {
		return Window{}, false
	}
	var window Window
	for frameIndex := 0; frameIndex < WindowFrames; frameIndex++ {
		frame := b.frames[(b.next+frameIndex)%WindowFrames]
		for subcarrier, value := range frame.values {
			window[subcarrier][frameIndex] = value
		}
	}
	return window, true
}

func (b *WindowBuffer) Reset() {
	if b == nil {
		return
	}
	b.frames = [WindowFrames]bufferedFrame{}
	b.count = 0
	b.next = 0
}
