package signal

import (
	"math"
	"sort"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
)

const (
	FrameHistoryCapacity = 100
	fieldGridSize        = 20
	historyResetAfter    = 5 * time.Second
)

type FeatureInfo struct {
	MeanRSSI           float64 `json:"mean_rssi"`
	Variance           float64 `json:"variance"`
	MotionBandPower    float64 `json:"motion_band_power"`
	BreathingBandPower float64 `json:"breathing_band_power"`
	DominantFreqHz     float64 `json:"dominant_freq_hz"`
	ChangePoints       int     `json:"change_points"`
	SpectralPower      float64 `json:"spectral_power"`
}

type Classification struct {
	MotionLevel string  `json:"motion_level"`
	Presence    bool    `json:"presence"`
	Confidence  float64 `json:"confidence"`
}

type SignalField struct {
	GridSize [3]int    `json:"grid_size"`
	Values   []float64 `json:"values"`
}

type Observation struct {
	Timestamp   time.Time      `json:"-"`
	NodeID      uint8          `json:"node_id"`
	RSSIDBm     int8           `json:"rssi_dbm"`
	Features    FeatureInfo    `json:"features"`
	Class       Classification `json:"classification"`
	Motion      float64        `json:"motion_score"`
	Quality     float64        `json:"signal_quality_score"`
	Field       SignalField    `json:"signal_field"`
	Amplitude   []float64      `json:"-"`
	Phase       []float64      `json:"-"`
	Subcarriers int            `json:"subcarrier_count"`
}

// Processor maintains a bounded per-node history and applies the same
// feature scaling, baseline tracking, EMA and 4-frame classification debounce
// as the existing sensing pipeline.
type Processor struct {
	frames             [][]float64
	meanHistory        []float64
	baselineMotion     float64
	baselineFrames     uint64
	smoothedMotion     float64
	currentMotionLevel string
	debounceCandidate  string
	debounceCounter    uint32
	last               time.Time
}

func NewProcessor() *Processor {
	return &Processor{currentMotionLevel: "absent", debounceCandidate: "absent"}
}

func (p *Processor) LastSeen() time.Time { return p.last }

func (p *Processor) Push(frame protocol.CSIFrame, now time.Time, sampleRateHz float64) Observation {
	if !p.last.IsZero() && now.Sub(p.last) > historyResetAfter {
		*p = Processor{currentMotionLevel: "absent", debounceCandidate: "absent"}
	}
	amp := append([]float64(nil), frame.Amplitude...)
	phase := append([]float64(nil), frame.Phase...)
	p.frames = append(p.frames, amp)
	if len(p.frames) > FrameHistoryCapacity {
		p.frames = p.frames[len(p.frames)-FrameHistoryCapacity:]
	}
	mean := mean(amp)
	p.meanHistory = append(p.meanHistory, mean)
	if len(p.meanHistory) > FrameHistoryCapacity {
		p.meanHistory = p.meanHistory[len(p.meanHistory)-FrameHistoryCapacity:]
	}
	p.last = now

	features, rawMotion, quality := extractFeatures(frame, p.frames, p.meanHistory, sampleRateHz)
	p.updateClassification(rawMotion)
	breathingHz := dominantBandFrequency(p.meanHistory, sampleRateHz, 0.1, 0.5)
	field := generateSignalField(p.smoothedMotion*100, breathingHz, quality, computeSubcarrierVariances(p.frames, len(amp)))
	return Observation{
		Timestamp: now,
		NodeID:    frame.NodeID,
		RSSIDBm:   frame.RSSIDBm,
		Features:  features,
		Class: Classification{
			MotionLevel: p.currentMotionLevel,
			Presence:    p.smoothedMotion > 0.03,
			Confidence:  clamp(0.4+p.smoothedMotion*0.6, 0, 1),
		},
		Motion:      p.smoothedMotion,
		Quality:     quality,
		Field:       field,
		Amplitude:   amp,
		Phase:       phase,
		Subcarriers: int(frame.Subcarriers),
	}
}

func (p *Processor) updateClassification(rawMotion float64) {
	p.baselineFrames++
	if p.baselineFrames < 50 {
		p.baselineMotion = p.baselineMotion*0.9 + rawMotion*0.1
	} else if rawMotion < p.smoothedMotion+0.05 {
		p.baselineMotion = p.baselineMotion*0.997 + rawMotion*0.003
	}
	adjusted := math.Max(0, rawMotion-p.baselineMotion*0.7)
	p.smoothedMotion = p.smoothedMotion*0.85 + adjusted*0.15
	candidate := rawClassify(p.smoothedMotion)
	if candidate == p.currentMotionLevel {
		p.debounceCounter = 0
		p.debounceCandidate = candidate
	} else if candidate == p.debounceCandidate {
		p.debounceCounter++
		if p.debounceCounter >= 4 {
			p.currentMotionLevel = candidate
			p.debounceCounter = 0
		}
	} else {
		p.debounceCandidate = candidate
		p.debounceCounter = 1
	}
}

func rawClassify(score float64) string {
	switch {
	case score > 0.25:
		return "active"
	case score > 0.12:
		return "present_moving"
	case score > 0.04:
		return "present_still"
	default:
		return "absent"
	}
}

func extractFeatures(frame protocol.CSIFrame, history [][]float64, means []float64, sampleRateHz float64) (FeatureInfo, float64, float64) {
	amp := frame.Amplitude
	if len(amp) == 0 {
		return FeatureInfo{MeanRSSI: float64(frame.RSSIDBm)}, 0, 0
	}
	weights := importanceWeights(amp)
	weightSum := sum(weights)
	meanAmp := 0.0
	if weightSum > 0 {
		for i, value := range amp {
			meanAmp += value * weights[i]
		}
		meanAmp /= weightSum
	} else {
		meanAmp = mean(amp)
	}
	intraVariance := 0.0
	for i, value := range amp {
		weight := 1.0
		if i < len(weights) && weightSum > 0 {
			weight = weights[i]
		}
		delta := value - meanAmp
		intraVariance += weight * delta * delta
	}
	if weightSum > 0 {
		intraVariance /= weightSum
	} else {
		intraVariance /= float64(len(amp))
	}
	variances := computeSubcarrierVariances(history, len(amp))
	temporalVariance := mean(variances)
	variance := math.Max(intraVariance, temporalVariance)
	spectralPower := 0.0
	for _, value := range amp {
		spectralPower += value * value
	}
	spectralPower /= float64(len(amp))
	half := len(amp) / 2
	motionBand, breathingBand := bandVariance(amp[half:], meanAmp), bandVariance(amp[:half], meanAmp)
	changePoints := 0
	threshold := meanAmp * 1.2
	for i := 1; i < len(amp); i++ {
		if (amp[i-1] < threshold) != (amp[i] < threshold) {
			changePoints++
		}
	}
	temporalMotion := 0.0
	if len(history) >= 2 {
		previous := history[len(history)-2]
		limit := min(len(amp), len(previous))
		if limit > 0 {
			diffEnergy := 0.0
			for i := 0; i < limit; i++ {
				delta := amp[i] - previous[i]
				diffEnergy += delta * delta
			}
			temporalMotion = clamp(math.Sqrt((diffEnergy/float64(limit))/(meanAmp*meanAmp+1e-9)), 0, 1)
		}
	} else {
		temporalMotion = clamp(math.Sqrt(intraVariance/(meanAmp*meanAmp+1e-9)), 0, 1)
	}
	varianceMotion := clamp(temporalVariance/10, 0, 1)
	mbpMotion := clamp(motionBand/25, 0, 1)
	cpMotion := clamp(float64(changePoints)/15, 0, 1)
	motion := clamp(0.4*temporalMotion+0.2*varianceMotion+0.25*mbpMotion+0.15*cpMotion, 0, 1)
	snrDB := math.Max(0, float64(frame.RSSIDBm-frame.NoiseFloor))
	snrQuality := clamp(snrDB/40, 0, 1)
	stability := math.Max(0, 1-clamp(temporalVariance/(meanAmp*meanAmp+1e-9), 0, 1))
	quality := clamp(0.6*snrQuality+0.4*stability, 0, 1)
	return FeatureInfo{
		MeanRSSI: float64(frame.RSSIDBm), Variance: variance,
		MotionBandPower: motionBand, BreathingBandPower: breathingBand,
		DominantFreqHz: dominantFrequency(means, sampleRateHz),
		ChangePoints:   changePoints, SpectralPower: spectralPower,
	}, motion, quality
}

func computeSubcarrierVariances(history [][]float64, n int) []float64 {
	if n <= 0 {
		return nil
	}
	variances := make([]float64, n)
	if len(history) == 0 {
		return variances
	}
	means, squares := make([]float64, n), make([]float64, n)
	for _, frame := range history {
		for k := 0; k < n; k++ {
			value := 0.0
			if k < len(frame) {
				value = frame[k]
			}
			means[k] += value
			squares[k] += value * value
		}
	}
	count := float64(len(history))
	for k := range variances {
		avg := means[k] / count
		variances[k] = math.Max(0, squares[k]/count-avg*avg)
	}
	return variances
}

func importanceWeights(sensitivity []float64) []float64 {
	if len(sensitivity) == 0 {
		return nil
	}
	sorted := append([]float64(nil), sensitivity...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	maxSensitivity := math.Max(1e-9, sorted[len(sorted)-1])
	weights := make([]float64, len(sensitivity))
	for i, value := range sensitivity {
		if value >= median {
			weights[i] = 1 + math.Min(value/maxSensitivity, 1)
		} else {
			weights[i] = 0.5
		}
	}
	return weights
}

func bandVariance(values []float64, center float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		delta := value - center
		sum += delta * delta
	}
	return sum / float64(len(values))
}

func dominantFrequency(samples []float64, sampleRateHz float64) float64 {
	return dominantBandFrequency(samples, sampleRateHz, 0.1, math.Min(4, sampleRateHz*0.45))
}

func dominantBandFrequency(samples []float64, sampleRateHz, low, high float64) float64 {
	if !isFinite(sampleRateHz) || sampleRateHz <= 0 || len(samples) < 16 || high < low || high < 0.1 {
		return 0
	}
	if high > sampleRateHz*0.5 {
		high = sampleRateHz * 0.5
	}
	meanValue := mean(samples)
	bestHz, bestPower, totalPower, bins := 0.0, 0.0, 0.0, 0
	for bin := 1; bin <= len(samples)/2; bin++ {
		freq := float64(bin) * sampleRateHz / float64(len(samples))
		if freq < low || freq > high {
			continue
		}
		realPart, imagPart := 0.0, 0.0
		for index, value := range samples {
			window := 1.0
			if len(samples) > 1 {
				window = 0.5 * (1 - math.Cos(2*math.Pi*float64(index)/float64(len(samples)-1)))
			}
			angle := 2 * math.Pi * float64(bin*index) / float64(len(samples))
			weighted := (value - meanValue) * window
			realPart += weighted * math.Cos(angle)
			imagPart -= weighted * math.Sin(angle)
		}
		power := realPart*realPart + imagPart*imagPart
		totalPower += power
		bins++
		if power > bestPower {
			bestPower, bestHz = power, freq
		}
	}
	if bins == 0 || totalPower/float64(bins) <= math.SmallestNonzeroFloat64 || bestPower/(totalPower/float64(bins)) < 2 {
		return 0
	}
	return bestHz
}

func generateSignalField(motionScore, breathingRateHz, quality float64, variances []float64) SignalField {
	grid := fieldGridSize
	values := make([]float64, grid*grid)
	center := (float64(grid) - 1) / 2
	maxVariance := 0.0
	for _, value := range variances {
		if value > maxVariance {
			maxVariance = value
		}
	}
	norm := maxVariance
	if norm <= 1e-9 {
		norm = 1
	}
	nSubs := max(1, len(variances))
	for k, variance := range variances {
		weight := variance / norm * motionScore
		if weight < 1e-6 {
			continue
		}
		angle := float64(k) / float64(nSubs) * 2 * math.Pi
		radius := center * 0.8 * math.Sqrt(weight)
		hx, hz := center+radius*math.Cos(angle), center+radius*math.Sin(angle)
		spread := math.Max(0.5, 0.5+weight*2)
		for z := 0; z < grid; z++ {
			for x := 0; x < grid; x++ {
				dx, dz := float64(x)-hx, float64(z)-hz
				values[z*grid+x] += weight * math.Exp(-(dx*dx+dz*dz)/(2*spread*spread))
			}
		}
	}
	for z := 0; z < grid; z++ {
		for x := 0; x < grid; x++ {
			dx, dz := float64(x)-center, float64(z)-center
			values[z*grid+x] += quality * math.Exp(-math.Hypot(dx, dz)*0.12) * 0.3
		}
	}
	if breathingRateHz > 0.05 {
		ringRadius, ringWidth := center*0.55, 1.8
		for z := 0; z < grid; z++ {
			for x := 0; x < grid; x++ {
				dist := math.Hypot(float64(x)-center, float64(z)-center)
				delta := dist - ringRadius
				values[z*grid+x] += 0.08 * math.Exp(-(delta*delta)/(2*ringWidth*ringWidth))
			}
		}
	}
	maximum := 0.0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	if maximum > 1e-9 {
		for i := range values {
			values[i] = clamp(values[i]/maximum, 0, 1)
		}
	}
	return SignalField{GridSize: [3]int{grid, 1, grid}, Values: values}
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func sum(values []float64) float64 {
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum
}

func clamp(value, low, high float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return low
	}
	return math.Max(low, math.Min(value, high))
}

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
