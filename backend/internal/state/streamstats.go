package state

import (
	"sort"
	"sync"
	"time"
)

const (
	updateRateWindow     = 5 * time.Second
	updateRateMaxSamples = 1024
)

type updateRateMeter struct {
	mu    sync.Mutex
	times []time.Time
}

func (m *updateRateMeter) Observe(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.times) == updateRateMaxSamples {
		copy(m.times, m.times[1:])
		m.times = m.times[:len(m.times)-1]
	}
	m.times = append(m.times, now)
	m.pruneLocked(now)
}

func (m *updateRateMeter) RateHz(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if len(m.times) < 2 {
		return 0
	}
	span := m.times[len(m.times)-1].Sub(m.times[0]).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(len(m.times)-1) / span
}

func (m *updateRateMeter) pruneLocked(now time.Time) {
	first := 0
	for first < len(m.times) {
		age := now.Sub(m.times[first])
		if age < 0 || age <= updateRateWindow {
			break
		}
		first++
	}
	if first > 0 {
		copy(m.times, m.times[first:])
		m.times = m.times[:len(m.times)-first]
	}
}

type StreamNodeRate struct {
	NodeID        uint8    `json:"node_id"`
	CSIFPS        *float64 `json:"csi_fps"`
	CSIFPSSamples uint32   `json:"csi_fps_samples"`
}

type StreamMetrics struct {
	Active      bool             `json:"active"`
	FPS         float64          `json:"fps"`
	CSIFPSTotal float64          `json:"csi_fps_total"`
	Nodes       []StreamNodeRate `json:"nodes"`
}

// StreamMetrics mirrors the current Rust /api/v1/stream/status payload. The
// aggregate rate is measured from emitted sensing updates, while per-node
// rates use only fresh CSI frames and are nil until the rate EMA has warmed up.
func (s *Store) StreamMetrics(now time.Time) StreamMetrics {
	if s == nil {
		return StreamMetrics{Nodes: []StreamNodeRate{}}
	}
	fps := roundOne(s.updateRate.RateHz(now))
	metrics := StreamMetrics{Active: fps > 0, FPS: fps, Nodes: []StreamNodeRate{}}
	s.nodesMu.RLock()
	pairs := make([]struct {
		id   uint8
		node *nodeRecord
	}, 0, len(s.nodes))
	for id, node := range s.nodes {
		pairs = append(pairs, struct {
			id   uint8
			node *nodeRecord
		}{id: id, node: node})
	}
	s.nodesMu.RUnlock()

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	for _, pair := range pairs {
		pair.node.mu.RLock()
		age := now.Sub(pair.node.lastFrameArrival)
		if pair.node.hasCSI && !pair.node.lastFrameArrival.IsZero() && age >= 0 && age <= nodeFreshness {
			node := StreamNodeRate{NodeID: pair.id, CSIFPSSamples: pair.node.csiFPSSamples}
			if pair.node.csiFPSSamples >= 5 {
				value := roundOne(min(max(pair.node.csiFPSEMA, 1), maxPhysicalSampleRateHz))
				node.CSIFPS = &value
				metrics.CSIFPSTotal += value
			}
			metrics.Nodes = append(metrics.Nodes, node)
		}
		pair.node.mu.RUnlock()
	}
	metrics.CSIFPSTotal = roundOne(metrics.CSIFPSTotal)
	return metrics
}

func roundOne(value float64) float64 {
	return float64(int64(value*10+0.5)) / 10
}
