// Package activity computes bounded MediaTek CSI fluctuation indices. These
// indices describe relative channel variation; they are not validated
// occupancy or person-count estimates.
package activity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	FastWindow            = 2 * time.Second
	SlowTimeConstant      = 300 * time.Second
	SlowFloor             = 1e-4
	PeakHoldWindow        = 30 * time.Second
	IndexMin              = -2.0
	IndexMax              = 3.0
	HistoryRetention      = 300 * time.Second
	HistorySampleInterval = time.Second
	RoomStaleAfter        = 30 * time.Second
	FloorDownTimeConstant = 600 * time.Second
	FloorUpCreepPerMinute = 0.01
	FloorInitWindow       = 30 * time.Second
	FloorInitPercentile   = 0.20
	maxFloorInitSamples   = 2048
	maxDevices            = 64
	maxHistorySamples     = 301
)

type Verdict struct {
	Index      float64 `json:"index"`
	Level      float64 `json:"level"`
	Fast       float64 `json:"fast"`
	Slow       float64 `json:"slow"`
	Peak30S    float64 `json:"peak_30s"`
	Frames2S   int     `json:"frames_2s"`
	Floor      float64 `json:"floor"`
	AbsLevel   float64 `json:"abs_level"`
	AbsPeak30S float64 `json:"abs_peak_30s"`
}

type point struct {
	at    time.Time
	value float64
}

// Tracker owns one device's slow baseline, quiet-room floor, and peak holds.
// It is not internally synchronized; Manager serializes all operations.
type Tracker struct {
	slow            float64
	hasSlow         bool
	floor           float64
	hasFloor        bool
	floorSamples    []float64
	floorInitStart  time.Time
	hasFloorInit    bool
	lastUpdate      time.Time
	hasLastUpdate   bool
	levelHistory    []point
	absLevelHistory []point
	lastVerdict     Verdict
	hasLastVerdict  bool
}

func (t *Tracker) SeedFloor(floor float64) bool {
	if t == nil || !finite(floor) || floor <= 0 {
		return false
	}
	t.floor = math.Max(floor, SlowFloor)
	t.hasFloor = true
	t.floorSamples = nil
	t.hasFloorInit = false
	return true
}

func (t *Tracker) Floor() (float64, bool) {
	if t == nil || !t.hasFloor {
		return 0, false
	}
	return t.floor, true
}

func (t *Tracker) LastUpdate() (time.Time, bool) {
	if t == nil || !t.hasLastUpdate {
		return time.Time{}, false
	}
	return t.lastUpdate, true
}

func (t *Tracker) LastVerdict() (Verdict, bool) {
	if t == nil || !t.hasLastVerdict {
		return Verdict{}, false
	}
	return t.lastVerdict, true
}

func (t *Tracker) Observe(now time.Time, fast float64, frames int) Verdict {
	if t == nil {
		return Verdict{}
	}
	if !finite(fast) || fast < SlowFloor {
		fast = SlowFloor
	}
	if frames < 0 {
		frames = 0
	}
	dt := 0.0
	if t.hasLastUpdate && now.After(t.lastUpdate) {
		dt = now.Sub(t.lastUpdate).Seconds()
	}
	if !t.hasSlow {
		t.slow, t.hasSlow = fast, true
	} else {
		t.slow = math.Max(SlowFloor, ema(t.slow, fast, dt, SlowTimeConstant.Seconds()))
	}
	index := clamp(math.Log2(fast/t.slow), IndexMin, IndexMax)
	level := indexToLevel(index)

	floor := t.observeFloor(now, fast, dt)
	absLevel := clamp(math.Log2(fast/floor)/5, 0, 1)
	t.lastUpdate, t.hasLastUpdate = now, true
	t.levelHistory = append(t.levelHistory, point{now, level})
	t.absLevelHistory = append(t.absLevelHistory, point{now, absLevel})
	t.levelHistory = prunePoints(t.levelHistory, now, PeakHoldWindow)
	t.absLevelHistory = prunePoints(t.absLevelHistory, now, PeakHoldWindow)
	verdict := Verdict{
		Index: index, Level: level, Fast: fast, Slow: t.slow,
		Peak30S: maxPoint(t.levelHistory), Frames2S: frames,
		Floor: floor, AbsLevel: absLevel, AbsPeak30S: maxPoint(t.absLevelHistory),
	}
	t.lastVerdict, t.hasLastVerdict = verdict, true
	return verdict
}

func (t *Tracker) observeFloor(now time.Time, fast, dt float64) float64 {
	if t.hasFloor {
		floor := t.floor
		if fast < floor {
			floor = ema(floor, fast, dt, FloorDownTimeConstant.Seconds())
		}
		floor *= math.Pow(1+FloorUpCreepPerMinute, dt/60)
		t.floor = math.Max(floor, SlowFloor)
		return t.floor
	}
	if !t.hasFloorInit {
		t.floorInitStart, t.hasFloorInit = now, true
	}
	if len(t.floorSamples) < maxFloorInitSamples {
		t.floorSamples = append(t.floorSamples, fast)
	}
	if now.Sub(t.floorInitStart) >= FloorInitWindow {
		samples := append([]float64(nil), t.floorSamples...)
		sort.Float64s(samples)
		idx := int(math.Round(float64(len(samples)-1) * FloorInitPercentile))
		t.floor = math.Max(samples[idx], SlowFloor)
		t.hasFloor = true
		t.floorSamples = nil
		return t.floor
	}
	floor := math.Inf(1)
	for _, sample := range t.floorSamples {
		floor = math.Min(floor, sample)
	}
	return math.Max(floor, SlowFloor)
}

func ema(current, target, dt, tau float64) float64 {
	if tau <= 0 {
		return target
	}
	alpha := 1 - math.Exp(-dt/tau)
	return current + alpha*(target-current)
}

func indexToLevel(index float64) float64 { return clamp((index-IndexMin)/(IndexMax-IndexMin), 0, 1) }

func prunePoints(points []point, now time.Time, retention time.Duration) []point {
	first := 0
	for first < len(points) && now.Sub(points[first].at) > retention {
		first++
	}
	if first == 0 {
		return points
	}
	copy(points, points[first:])
	return points[:len(points)-first]
}

func maxPoint(points []point) float64 {
	maximum := 0.0
	for _, p := range points {
		maximum = math.Max(maximum, p.value)
	}
	return maximum
}

func clamp(value, minValue, maxValue float64) float64 {
	if math.IsNaN(value) {
		return minValue
	}
	return math.Max(minValue, math.Min(maxValue, value))
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

type HistorySample struct {
	AgeMS        uint64                 `json:"age_ms"`
	RoomLevel    float64                `json:"room_level"`
	RoomAbsLevel float64                `json:"room_abs_level"`
	Devices      map[string]DeviceLevel `json:"devices"`
}

type DeviceLevel struct {
	Level    float64 `json:"level"`
	AbsLevel float64 `json:"abs_level"`
}

type timedSample struct {
	at           time.Time
	roomLevel    float64
	roomAbsLevel float64
	devices      map[string]DeviceLevel
}

// Manager owns per-device trackers and a 1Hz-ish, five-minute room history.
type Manager struct {
	mu         sync.RWMutex
	devices    map[string]*Tracker
	history    []timedSample
	lastSample time.Time
	hasSample  bool
}

func NewManager() *Manager { return &Manager{devices: make(map[string]*Tracker)} }

func (m *Manager) Remove(deviceID string) {
	if m == nil || deviceID == "" {
		return
	}
	m.mu.Lock()
	delete(m.devices, deviceID)
	m.mu.Unlock()
}

func (m *Manager) Observe(deviceID string, now time.Time, fast float64, frames int) (Verdict, bool) {
	if m == nil || deviceID == "" {
		return Verdict{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tracker := m.devices[deviceID]
	if tracker == nil {
		if len(m.devices) >= maxDevices {
			m.evictOldest()
		}
		tracker = &Tracker{}
		m.devices[deviceID] = tracker
	}
	verdict := tracker.Observe(now, fast, frames)
	m.sample(now)
	return verdict, true
}

func (m *Manager) evictOldest() {
	oldestID := ""
	var oldest time.Time
	for id, tracker := range m.devices {
		seen, ok := tracker.LastUpdate()
		if !ok || oldestID == "" || seen.Before(oldest) {
			oldestID, oldest = id, seen
		}
	}
	if oldestID != "" {
		delete(m.devices, oldestID)
	}
}

func (m *Manager) roomValues(now time.Time) (float64, float64, map[string]DeviceLevel) {
	weightedIndex, totalWeight, plainIndex, freshCount := 0.0, 0.0, 0.0, 0
	roomAbs := 0.0
	levels := make(map[string]DeviceLevel)
	for id, tracker := range m.devices {
		lastUpdate, ok := tracker.LastUpdate()
		if !ok || now.Sub(lastUpdate) > RoomStaleAfter || now.Before(lastUpdate) {
			continue
		}
		verdict, ok := tracker.LastVerdict()
		if !ok {
			continue
		}
		freshCount++
		plainIndex += verdict.Index
		weightedIndex += verdict.Index * float64(verdict.Frames2S)
		totalWeight += float64(verdict.Frames2S)
		roomAbs = math.Max(roomAbs, verdict.AbsLevel)
		levels[id] = DeviceLevel{Level: verdict.Level, AbsLevel: verdict.AbsLevel}
	}
	if freshCount == 0 {
		return indexToLevel(IndexMin), 0, levels
	}
	index := plainIndex / float64(freshCount)
	if totalWeight > 0 {
		index = weightedIndex / totalWeight
	}
	return indexToLevel(clamp(index, IndexMin, IndexMax)), roomAbs, levels
}

func (m *Manager) sample(now time.Time) {
	if m.hasSample && now.Sub(m.lastSample) < HistorySampleInterval {
		return
	}
	roomLevel, roomAbsLevel, devices := m.roomValues(now)
	m.history = append(m.history, timedSample{at: now, roomLevel: roomLevel, roomAbsLevel: roomAbsLevel, devices: devices})
	m.lastSample, m.hasSample = now, true
	first := 0
	for first < len(m.history) && (now.Sub(m.history[first].at) > HistoryRetention || len(m.history)-first > maxHistorySamples) {
		first++
	}
	if first > 0 {
		copy(m.history, m.history[first:])
		m.history = m.history[:len(m.history)-first]
	}
}

func (m *Manager) History(now time.Time, seconds uint64) []HistorySample {
	if m == nil {
		return []HistorySample{}
	}
	window := time.Duration(0)
	if seconds > uint64(HistoryRetention/time.Second) {
		window = HistoryRetention
	} else {
		window = time.Duration(seconds) * time.Second
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	samples := make([]HistorySample, 0, len(m.history))
	for _, sample := range m.history {
		age := now.Sub(sample.at)
		if age < 0 || age > window {
			continue
		}
		devices := make(map[string]DeviceLevel, len(sample.devices))
		for id, level := range sample.devices {
			devices[id] = level
		}
		samples = append(samples, HistorySample{
			AgeMS: uint64(age.Milliseconds()), RoomLevel: sample.roomLevel,
			RoomAbsLevel: sample.roomAbsLevel, Devices: devices,
		})
	}
	return samples
}

func (m *Manager) SeedFloors(floors map[string]float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, floor := range floors {
		if id == "" || !finite(floor) || floor <= 0 {
			continue
		}
		tracker := m.devices[id]
		if tracker == nil {
			if len(m.devices) >= maxDevices {
				m.evictOldest()
			}
			tracker = &Tracker{}
			m.devices[id] = tracker
		}
		tracker.SeedFloor(floor)
	}
}

func (m *Manager) Floors() map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	floors := make(map[string]float64)
	for id, tracker := range m.devices {
		if floor, ok := tracker.Floor(); ok {
			floors[id] = floor
		}
	}
	return floors
}

const maxFloorStateBytes = 64 << 10

// LoadFloors reads the bounded {device_id: floor} restart state. Missing files
// mean first run; malformed state fails closed to the caller, which can log
// and start a fresh baseline without using untrusted values.
func LoadFloors(path string) (map[string]float64, error) {
	if path == "" {
		return nil, errors.New("activity-state path is empty")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]float64{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat activity state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFloorStateBytes {
		return nil, errors.New("activity state must be a regular file no larger than 64 KiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read activity state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var values map[string]json.Number
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode activity state: %w", err)
	}
	if values == nil {
		return nil, errors.New("activity state must be a JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("activity state contains trailing JSON data")
	}
	if len(values) > maxDevices {
		return nil, fmt.Errorf("activity state exceeds %d device entries", maxDevices)
	}
	floors := make(map[string]float64, len(values))
	for id, raw := range values {
		value, err := raw.Float64()
		if id == "" || len(id) > 256 || err != nil || !finite(value) || value <= 0 {
			return nil, errors.New("activity state contains an invalid device ID or floor")
		}
		floors[id] = value
	}
	return floors, nil
}

// WriteFloors atomically replaces restart state with a private file and syncs
// both file data and the containing directory before returning.
func WriteFloors(path string, floors map[string]float64) error {
	if path == "" || len(floors) > maxDevices {
		return errors.New("activity-state path is empty or device count exceeds limit")
	}
	for id, value := range floors {
		if id == "" || len(id) > 256 || !finite(value) || value <= 0 {
			return errors.New("activity state contains an invalid device ID or floor")
		}
	}
	data, err := json.MarshalIndent(floors, "", "  ")
	if err != nil {
		return fmt.Errorf("encode activity state: %w", err)
	}
	if len(data) > maxFloorStateBytes {
		return errors.New("activity state exceeds 64 KiB")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create activity state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".activity-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create activity state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure activity state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write activity state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync activity state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close activity state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace activity state: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open activity state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync activity state directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close activity state directory: %w", err)
	}
	return nil
}
