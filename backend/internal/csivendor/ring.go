package csivendor

import (
	"math"
	"time"
)

const (
	MaxRingFrames        = 256
	MaxRingBytes         = 512 << 10
	maxRingFrameOverhead = 128
)

type Chain struct {
	TX        uint8     `json:"tx"`
	RX        uint8     `json:"rx"`
	Amplitude []float32 `json:"-"`
	Phase     []float32 `json:"-"`
}

type CSIFrame struct {
	TimestampUS     uint64  `json:"ts_us"`
	Sequence        uint32  `json:"seq"`
	RSSIDBm         []int8  `json:"rssi"`
	SubcarrierCount uint16  `json:"-"`
	Chains          []Chain `json:"-"`
}

type ChainPosition struct {
	TX uint8 `json:"tx"`
	RX uint8 `json:"rx"`
}

// Ring keeps newest per-subcarrier CSI frames under both a frame-count and a
// byte budget. Each ring is protected by the owning state store's lock.
type Ring struct {
	frames          []CSIFrame
	bytes           int
	subcarrierCount uint16
}

type ChainAmplitudeStats struct {
	TX              uint8     `json:"tx"`
	RX              uint8     `json:"rx"`
	AmplitudeMean   []float32 `json:"amplitude_mean"`
	AmplitudeStd    []float32 `json:"amplitude_std"`
	FramesWithChain int       `json:"frames_with_chain"`
}

type RSSIMean struct {
	RX          uint8   `json:"rx"`
	MeanRSSIDBm float64 `json:"mean_rssi_dbm"`
}

type RingSummary struct {
	FrameCount      int
	SpanMS          uint64
	FrameRateHz     float64
	SubcarrierCount uint16
	RSSIByRX        []RSSIMean
	Chains          []ChainAmplitudeStats
}

func (r *Ring) Push(frame CSIFrame) {
	if r == nil {
		return
	}
	frameBytes := frameSize(frame)
	if frameBytes > MaxRingBytes {
		return
	}
	for len(r.frames) > 0 && (len(r.frames) >= MaxRingFrames || r.bytes+frameBytes > MaxRingBytes) {
		r.bytes -= frameSize(r.frames[0])
		copy(r.frames, r.frames[1:])
		r.frames[len(r.frames)-1] = CSIFrame{}
		r.frames = r.frames[:len(r.frames)-1]
	}
	r.frames = append(r.frames, cloneCSIFrame(frame))
	r.bytes += frameBytes
	r.subcarrierCount = frame.SubcarrierCount
}

func (r *Ring) Len() int {
	if r == nil {
		return 0
	}
	return len(r.frames)
}

func (r *Ring) ByteSize() int {
	if r == nil {
		return 0
	}
	return r.bytes
}

func (r *Ring) SubcarrierCount() uint16 {
	if r == nil {
		return 0
	}
	return r.subcarrierCount
}

func (r *Ring) Summary() RingSummary {
	if r == nil {
		return RingSummary{}
	}
	summary := RingSummary{
		FrameCount: len(r.frames), SubcarrierCount: r.subcarrierCount,
		Chains: []ChainAmplitudeStats{}, RSSIByRX: []RSSIMean{},
	}
	if len(r.frames) > 1 {
		oldest, newest := r.frames[0].TimestampUS, r.frames[len(r.frames)-1].TimestampUS
		spanUS := uint64(0)
		if newest > oldest {
			spanUS = newest - oldest
		}
		summary.SpanMS = spanUS / 1000
		if spanUS > 0 {
			summary.FrameRateHz = float64(len(r.frames)-1) / (float64(spanUS) / 1_000_000)
		}
	}
	subcarriers := int(r.subcarrierCount)
	positions := ChainUnion(r.frames)
	for _, position := range positions {
		sums := make([]float64, subcarriers)
		sumsq := make([]float64, subcarriers)
		count := 0
		for _, frame := range r.frames {
			var chain *Chain
			for i := range frame.Chains {
				if frame.Chains[i].TX == position.TX && frame.Chains[i].RX == position.RX {
					chain = &frame.Chains[i]
					break
				}
			}
			if chain == nil {
				continue
			}
			count++
			for i, amplitude := range chain.Amplitude {
				if i >= subcarriers {
					break
				}
				value := float64(amplitude)
				sums[i] += value
				sumsq[i] += value * value
			}
		}
		denominator := float64(max(count, 1))
		mean := make([]float32, subcarriers)
		std := make([]float32, subcarriers)
		for i := range mean {
			mean[i] = float32(sums[i] / denominator)
			variance := sumsq[i]/denominator - float64(mean[i])*float64(mean[i])
			if variance < 0 {
				variance = 0
			}
			std[i] = float32(math.Sqrt(variance))
		}
		summary.Chains = append(summary.Chains, ChainAmplitudeStats{
			TX: position.TX, RX: position.RX, AmplitudeMean: mean,
			AmplitudeStd: std, FramesWithChain: count,
		})
	}
	if len(r.frames) > 0 {
		rssiCount := len(r.frames[len(r.frames)-1].RSSIDBm)
		for rx := 0; rx < rssiCount; rx++ {
			var sum int64
			count := 0
			for _, frame := range r.frames {
				if rx < len(frame.RSSIDBm) {
					sum += int64(frame.RSSIDBm[rx])
					count++
				}
			}
			mean := 0.0
			if count > 0 {
				mean = float64(sum) / float64(count)
			}
			summary.RSSIByRX = append(summary.RSSIByRX, RSSIMean{RX: uint8(rx), MeanRSSIDBm: mean})
		}
	}
	return summary
}

// FastActivity returns the recent mean coefficient of variation across all
// observed (tx, rx, subcarrier) amplitude series and the number of frames in
// the timestamp window. It is a relative channel-fluctuation signal, not a
// person-count estimate.
func (r *Ring) FastActivity(window time.Duration) (float64, int) {
	if r == nil || len(r.frames) == 0 || window < 0 {
		return 0, 0
	}
	newest := r.frames[len(r.frames)-1].TimestampUS
	windowUS := uint64(window.Microseconds())
	first := len(r.frames)
	for first > 0 {
		timestamp := r.frames[first-1].TimestampUS
		age := uint64(0)
		if newest > timestamp {
			age = newest - timestamp
		}
		if age > windowUS {
			break
		}
		first--
	}
	recent := r.frames[first:]
	if len(recent) < 2 {
		return 0, len(recent)
	}
	subcarriers := int(r.subcarrierCount)
	var variationSum float64
	variationCount := 0
	for _, position := range ChainUnion(recent) {
		samples := make([][]float64, subcarriers)
		for _, frame := range recent {
			for _, chain := range frame.Chains {
				if chain.TX != position.TX || chain.RX != position.RX {
					continue
				}
				for k := 0; k < subcarriers && k < len(chain.Amplitude); k++ {
					samples[k] = append(samples[k], float64(chain.Amplitude[k]))
				}
				break
			}
		}
		for _, values := range samples {
			if len(values) < 2 {
				continue
			}
			var sum, sumSquares float64
			for _, value := range values {
				sum += value
				sumSquares += value * value
			}
			mean := sum / float64(len(values))
			variance := math.Max(0, sumSquares/float64(len(values))-mean*mean)
			variationSum += math.Sqrt(variance) / math.Max(math.Abs(mean), 1e-6)
			variationCount++
		}
	}
	if variationCount == 0 {
		return 0, len(recent)
	}
	return variationSum / float64(variationCount), len(recent)
}

// Tail returns the newest n frames in chronological order as an isolated copy.
func (r *Ring) Tail(n int) []CSIFrame {
	if r == nil || n <= 0 {
		return []CSIFrame{}
	}
	if n > len(r.frames) {
		n = len(r.frames)
	}
	frames := make([]CSIFrame, n)
	start := len(r.frames) - n
	for i := range frames {
		frames[i] = cloneCSIFrame(r.frames[start+i])
	}
	return frames
}

func ChainUnion(frames []CSIFrame) []ChainPosition {
	seen := make(map[ChainPosition]struct{})
	positions := make([]ChainPosition, 0)
	for _, frame := range frames {
		for _, chain := range frame.Chains {
			position := ChainPosition{TX: chain.TX, RX: chain.RX}
			if _, exists := seen[position]; exists {
				continue
			}
			seen[position] = struct{}{}
			positions = append(positions, position)
		}
	}
	return positions
}

func frameSize(frame CSIFrame) int {
	bytes := 24 + len(frame.RSSIDBm) + len(frame.Chains)*maxRingFrameOverhead
	for _, chain := range frame.Chains {
		bytes += (len(chain.Amplitude) + len(chain.Phase)) * 4
	}
	return bytes
}

func cloneCSIFrame(frame CSIFrame) CSIFrame {
	clone := frame
	clone.RSSIDBm = append([]int8(nil), frame.RSSIDBm...)
	clone.Chains = make([]Chain, len(frame.Chains))
	for i, chain := range frame.Chains {
		clone.Chains[i] = Chain{
			TX: chain.TX, RX: chain.RX,
			Amplitude: append([]float32(nil), chain.Amplitude...),
			Phase:     append([]float32(nil), chain.Phase...),
		}
	}
	return clone
}

func (frame CSIFrame) Clone() CSIFrame { return cloneCSIFrame(frame) }

func frameFromIQ(snapshot Snapshot, values [][2]float32, scale float32) *CSIFrame {
	if snapshot.SubcarrierCount == 0 || len(values) != snapshot.ElementCount || len(values) == 0 {
		return nil
	}
	subcarriers := int(snapshot.SubcarrierCount)
	frame := &CSIFrame{
		TimestampUS: snapshot.TimestampUS, Sequence: snapshot.Sequence,
		RSSIDBm:         append([]int8(nil), snapshot.RSSIDBm...),
		SubcarrierCount: snapshot.SubcarrierCount,
		Chains:          make([]Chain, 0, int(snapshot.TXCount)*int(snapshot.RXCount)),
	}
	for tx := 0; tx < int(snapshot.TXCount); tx++ {
		for rx := 0; rx < int(snapshot.RXCount); rx++ {
			base := (tx*int(snapshot.RXCount) + rx) * subcarriers
			chain := Chain{TX: uint8(tx), RX: uint8(rx), Amplitude: make([]float32, subcarriers), Phase: make([]float32, subcarriers)}
			for subcarrier := 0; subcarrier < subcarriers; subcarrier++ {
				real, imag := values[base+subcarrier][0], values[base+subcarrier][1]
				chain.Amplitude[subcarrier] = float32(math.Hypot(float64(real), float64(imag))) * scale
				chain.Phase[subcarrier] = float32(math.Atan2(float64(imag), float64(real)))
			}
			unwrapPhase(chain.Phase)
			frame.Chains = append(frame.Chains, chain)
		}
	}
	return frame
}

func unwrapPhase(phase []float32) {
	const twoPi float32 = 2 * math.Pi
	for i := 1; i < len(phase); i++ {
		delta := phase[i] - phase[i-1]
		for delta > math.Pi {
			phase[i] -= twoPi
			delta -= twoPi
		}
		for delta < -math.Pi {
			phase[i] += twoPi
			delta += twoPi
		}
	}
}
