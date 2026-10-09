package pose

import (
	"errors"
	"math"
	"sort"
	"time"
)

const (
	maxTracks       = 8
	maxMissedFrames = 5
	maxTrackAge     = 2 * time.Second
)

type Detection struct {
	Center [2]float64 `json:"center"`
	Space  string     `json:"coordinate_space"`
}

type Track struct {
	ID        uint64     `json:"track_id"`
	Center    [2]float64 `json:"center"`
	Space     string     `json:"coordinate_space"`
	FirstSeen time.Time  `json:"first_seen"`
	LastSeen  time.Time  `json:"last_seen"`
	Hits      int        `json:"hits"`
	Missed    int        `json:"missed_frames"`
	Confirmed bool       `json:"confirmed"`
}

type Tracker struct {
	tracks  []Track
	nextID  uint64
	lastAt  time.Time
	maxStep float64
}

func NewTracker(maxStep float64) (*Tracker, error) {
	if !finite(maxStep) || maxStep <= 0 || maxStep > 100 {
		return nil, errors.New("track association distance must be finite and within (0, 100]")
	}
	return &Tracker{maxStep: maxStep}, nil
}

// Update associates detections globally with a Hungarian assignment. The
// caller supplies an explicit coordinate space; image-normalized and metric
// positions are never compared with one another.
func (t *Tracker) Update(at time.Time, detections []Detection) ([]Track, error) {
	if t == nil || at.IsZero() {
		return nil, errors.New("tracker and timestamp are required")
	}
	if !t.lastAt.IsZero() && !at.After(t.lastAt) {
		return nil, errors.New("pose timestamps must increase monotonically")
	}
	if len(detections) > maxTracks {
		return nil, errors.New("pose detector exceeded the configured person limit")
	}
	for _, detection := range detections {
		if detection.Space == "" || !finite(detection.Center[0]) || !finite(detection.Center[1]) ||
			math.Abs(detection.Center[0]) > 1e6 || math.Abs(detection.Center[1]) > 1e6 {
			return nil, errors.New("pose detector returned an invalid center or coordinate space")
		}
	}
	t.lastAt = at
	t.tracks = retainFresh(t.tracks, at)
	assignments := minimumCostAssignment(t.tracks, detections, t.maxStep)
	matchedDetection := make([]bool, len(detections))
	matchedTrack := make([]bool, len(t.tracks))
	for trackIndex, detectionIndex := range assignments {
		if detectionIndex < 0 {
			continue
		}
		track := &t.tracks[trackIndex]
		detection := detections[detectionIndex]
		track.Center, track.Space, track.LastSeen = detection.Center, detection.Space, at
		track.Hits++
		track.Missed = 0
		track.Confirmed = track.Hits >= 2
		matchedTrack[trackIndex], matchedDetection[detectionIndex] = true, true
	}
	for i := range t.tracks {
		if !matchedTrack[i] {
			t.tracks[i].Missed++
		}
	}
	for i, detection := range detections {
		if matchedDetection[i] {
			continue
		}
		t.nextID++
		t.tracks = append(t.tracks, Track{
			ID: t.nextID, Center: detection.Center, Space: detection.Space,
			FirstSeen: at, LastSeen: at, Hits: 1,
		})
	}
	t.tracks = retainFresh(t.tracks, at)
	out := append([]Track(nil), t.tracks...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *Tracker) Reset() {
	if t == nil {
		return
	}
	t.tracks = nil
	t.lastAt = time.Time{}
}

func retainFresh(tracks []Track, now time.Time) []Track {
	kept := tracks[:0]
	for _, track := range tracks {
		if track.Missed < maxMissedFrames && now.Sub(track.LastSeen) <= maxTrackAge {
			kept = append(kept, track)
		}
	}
	return kept
}

func minimumCostAssignment(tracks []Track, detections []Detection, maxStep float64) []int {
	assignments := make([]int, len(tracks))
	for i := range assignments {
		assignments[i] = -1
	}
	if len(tracks) == 0 || len(detections) == 0 {
		return assignments
	}
	n := len(tracks) + len(detections)
	cost := make([][]float64, n)
	for i := range cost {
		cost[i] = make([]float64, n)
		for j := range cost[i] {
			cost[i][j] = maxStep
		}
	}
	for i := 0; i < len(tracks); i++ {
		for j := len(detections); j < n; j++ {
			cost[i][j] = maxStep
		}
	}
	for i, track := range tracks {
		for j, detection := range detections {
			if track.Space != detection.Space {
				cost[i][j] = maxStep * 1e6
				continue
			}
			distance := math.Hypot(track.Center[0]-detection.Center[0], track.Center[1]-detection.Center[1])
			if distance > maxStep {
				cost[i][j] = maxStep * 1e6
			} else {
				cost[i][j] = distance
			}
		}
	}
	for i := len(tracks); i < n; i++ {
		for j := 0; j < n; j++ {
			cost[i][j] = 0
		}
	}
	assignment := hungarian(cost)
	for i := range tracks {
		j := assignment[i]
		if j >= 0 && j < len(detections) && cost[i][j] <= maxStep {
			assignments[i] = j
		}
	}
	return assignments
}

// hungarian returns one assigned column for each row in a square cost matrix.
func hungarian(cost [][]float64) []int {
	n := len(cost)
	u, v := make([]float64, n+1), make([]float64, n+1)
	p, way := make([]int, n+1), make([]int, n+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, n+1)
		used := make([]bool, n+1)
		for j := 1; j <= n; j++ {
			minv[j] = math.Inf(1)
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta, j1 := math.Inf(1), 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				current := cost[i0-1][j-1] - u[i0] - v[j]
				if current < minv[j] {
					minv[j], way[j] = current, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	assignment := make([]int, n)
	for i := range assignment {
		assignment[i] = -1
	}
	for j := 1; j <= n; j++ {
		if p[j] != 0 {
			assignment[p[j]-1] = j - 1
		}
	}
	return assignment
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
