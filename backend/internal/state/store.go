package state

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruvnet/ruview/backend/internal/activity"
	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/calibration"
	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/inference"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/multistatic"
	posemodel "github.com/ruvnet/ruview/backend/internal/pose"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/signal"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

const (
	maxTrackedNodes         = 64
	maxMediaTekCSIDevices   = 64
	nodeFreshness           = 5 * time.Second
	latestStale             = 10 * time.Second
	roomDebounceDuration    = 1500 * time.Millisecond
	meshSyncMaxAge          = 9 * time.Second
	mediaTekDeviceFreshness = 30 * time.Second
	defaultSampleRateHz     = 20.0
	maxPhysicalSampleRateHz = 50.0
	sampleRateEMAAlpha      = 1.0 / 32.0
)

type NodeInfo struct {
	NodeID            uint8         `json:"node_id"`
	RSSIDBm           float64       `json:"rssi_dbm"`
	Position          [3]float64    `json:"position"`
	PositionAvailable bool          `json:"position_available"`
	Amplitude         []float64     `json:"amplitude"`
	SubcarrierCount   int           `json:"subcarrier_count"`
	MeshTimestampUS   *uint64       `json:"mesh_timestamp_us,omitempty"`
	Sync              *SyncSnapshot `json:"sync,omitempty"`
}

// SyncSnapshot mirrors the mesh-clock state emitted by the Rust backend.
// Staleness is measured at serialization time and does not imply that the
// node's clock offset is still safe for timestamp recovery.
type SyncSnapshot struct {
	OffsetUS      int64   `json:"offset_us"`
	IsLeader      bool    `json:"is_leader"`
	IsValid       bool    `json:"is_valid"`
	Smoothed      bool    `json:"smoothed"`
	Sequence      uint32  `json:"sequence"`
	CSIFPSEMA     float64 `json:"csi_fps_ema"`
	CSIFPSSamples uint32  `json:"csi_fps_samples"`
	StalenessMS   *uint64 `json:"staleness_ms,omitempty"`
}

// NodeStatus is the management API view of a tracked sensor, including nodes
// that have only sent a mesh sync beacon and have not sent CSI yet.
type NodeStatus struct {
	NodeID             uint8   `json:"node_id"`
	Status             string  `json:"status"`
	LastSeenMS         uint64  `json:"last_seen_ms"`
	CSIStatus          string  `json:"csi_status"`
	CSILastSeenMS      *uint64 `json:"csi_last_seen_ms"`
	CSISequence        *uint32 `json:"csi_sequence"`
	RSSIDBm            float64 `json:"rssi_dbm"`
	MotionLevel        string  `json:"motion_level"`
	PersonCount        uint8   `json:"person_count"`
	PersonCountValid   *bool   `json:"person_count_valid"`
	PositionConfigured bool    `json:"position_configured"`
}

type NodeFeatureInfo struct {
	NodeID         uint8                 `json:"node_id"`
	Features       signal.FeatureInfo    `json:"features"`
	Classification signal.Classification `json:"classification"`
	MotionScore    float64               `json:"motion_score"`
	LastSeenMS     uint64                `json:"last_seen_ms"`
}

type SensingUpdate struct {
	Type                         string                `json:"type"`
	Timestamp                    float64               `json:"timestamp"`
	Source                       string                `json:"source"`
	InputMode                    string                `json:"input_mode,omitempty"`
	MeasurementAuthority         string                `json:"measurement_authority"`
	CalibratedEvidenceAuthorized bool                  `json:"calibrated_evidence_authorized"`
	NumericVitalsAuthorized      bool                  `json:"numeric_vitals_authorized"`
	Tick                         uint64                `json:"tick"`
	Nodes                        []NodeInfo            `json:"nodes"`
	Features                     signal.FeatureInfo    `json:"features"`
	Classification               signal.Classification `json:"classification"`
	SignalField                  signal.SignalField    `json:"signal_field"`
	SignalQualityScore           *float64              `json:"signal_quality_score,omitempty"`
	Persons                      []any                 `json:"persons,omitempty"`
	EstimatedPersons             *uint8                `json:"estimated_persons,omitempty"`
	NodeFeatures                 []NodeFeatureInfo     `json:"node_features,omitempty"`
	SpatialFusion                multistatic.Snapshot  `json:"spatial_fusion"`
	observedAt                   time.Time
}

type PoseEstimate struct {
	Timestamp  time.Time            `json:"timestamp"`
	NodeID     uint8                `json:"node_id"`
	Source     string               `json:"source"`
	Evidence   string               `json:"evidence"`
	Prediction posemodel.Prediction `json:"prediction"`
}

type PoseInferenceSnapshot struct {
	Enabled         bool                `json:"enabled"`
	Status          string              `json:"status"`
	Reason          string              `json:"reason"`
	Model           posemodel.ModelInfo `json:"model"`
	SelectedNodeID  *uint8              `json:"selected_node_id,omitempty"`
	FramesProcessed uint64              `json:"frames_processed"`
	Estimate        *PoseEstimate       `json:"estimate,omitempty"`
}

type nodeRecord struct {
	mu                 sync.RWMutex
	processor          *signal.Processor
	observation        *signal.Observation
	featureState       *protocol.FeatureStatePacket
	edgeVitals         *protocol.VitalsPacket
	lastSeen           time.Time
	featureSeen        time.Time
	vitalsSeen         time.Time
	latestSync         *protocol.SyncPacket
	syncSeen           time.Time
	meshTimestampUS    *uint64
	lastFrameArrival   time.Time
	csiFPSEMA          float64
	csiFPSSamples      uint32
	latestCSISequence  uint32
	hasCSI             bool
	arrivalTimestampUS uint64
	rssiDBm            float64
	hasRSSI            bool
	position           [3]float64
	positionAvailable  bool
	source             string
	inputMode          string
	fusionConfidence   uint8
}

type nodeView struct {
	id                 uint8
	observation        *signal.Observation
	featureState       *protocol.FeatureStatePacket
	edgeVitals         *protocol.VitalsPacket
	lastSeen           time.Time
	featureSeen        time.Time
	vitalsSeen         time.Time
	sync               *SyncSnapshot
	meshTimestampUS    *uint64
	position           [3]float64
	positionAvailable  bool
	sequence           uint32
	arrivalTimestampUS uint64
	source             string
	inputMode          string
}

type Store struct {
	sourceMu                sync.RWMutex
	source                  string
	activeSource            string
	activeSeen              time.Time
	started                 time.Time
	nodesMu                 sync.RWMutex
	nodes                   map[uint8]*nodeRecord
	nodePositions           map[uint8][3]float64
	latest                  atomic.Pointer[SensingUpdate]
	tick                    atomic.Uint64
	lastInputAt             atomic.Int64
	frames                  atomic.Uint64
	updateRate              updateRateMeter
	vitals                  atomic.Uint64
	badCounts               atomic.Uint64
	syncPackets             atomic.Uint64
	hub                     *ws.Hub
	updateMu                sync.Mutex
	lastRefreshAt           time.Time
	roomDebouncedLevel      string
	roomDebounceCandidate   string
	roomDebounceSince       time.Time
	adaptiveMu              sync.RWMutex
	adaptiveModel           *adaptive.Model
	adaptiveErrors          atomic.Uint64
	rvfInferenceErrors      atomic.Uint64
	rvfModelsMu             sync.RWMutex
	rvfModels               *model.Registry
	wasmMu                  sync.RWMutex
	wasmLatest              *protocol.WASMOutputPacket
	vendorCSIMu             sync.RWMutex
	vendorCSI               map[string]vendorCSIRecord
	mediatekCSIByDevice     map[string]vendorCSIRecord
	mediatekCSIRingByDevice map[string]*csivendor.Ring
	latestRadar             *radar.Snapshot
	mediatekActivity        *activity.Manager
	calibrationMu           sync.RWMutex
	calibration             *calibration.Manager
	poseMu                  sync.RWMutex
	poseModel               *posemodel.Model
	poseNodeID              *uint8
	poseWindows             map[uint8]*posemodel.WindowBuffer
	poseLatest              *PoseEstimate
	poseFramesProcessed     uint64
	poseStatus              string
	poseReason              string
}

type vendorCSIRecord struct {
	snapshot csivendor.Snapshot
	seenAt   time.Time
}

type MediaTekDevice struct {
	Snapshot csivendor.Snapshot
	AgeMS    uint64
}

func New(source string, hub *ws.Hub) *Store {
	if hub == nil {
		hub = ws.NewHub()
	}
	return &Store{
		source: source, started: time.Now(), nodes: make(map[uint8]*nodeRecord), nodePositions: make(map[uint8][3]float64), hub: hub,
		roomDebouncedLevel: "absent", roomDebounceCandidate: "absent",
		vendorCSI: make(map[string]vendorCSIRecord), mediatekCSIByDevice: make(map[string]vendorCSIRecord),
		mediatekCSIRingByDevice: make(map[string]*csivendor.Ring), mediatekActivity: activity.NewManager(),
		poseWindows: make(map[uint8]*posemodel.WindowBuffer), poseStatus: "disabled", poseReason: "pose_model_not_configured",
	}
}

// SetPoseModel configures experimental single-person image-space inference.
// A nil model disables it. The model's output is never interpreted as a
// person detection or a metric coordinate by this store.
func (s *Store) SetPoseModel(model *posemodel.Model, nodeID *uint8) error {
	if s == nil {
		return errors.New("nil state store")
	}
	if nodeID != nil {
		value := *nodeID
		nodeID = &value
	}
	s.poseMu.Lock()
	defer s.poseMu.Unlock()
	s.poseModel, s.poseNodeID = model, nodeID
	s.poseWindows = make(map[uint8]*posemodel.WindowBuffer)
	s.poseLatest = nil
	s.poseFramesProcessed = 0
	if model == nil {
		s.poseStatus, s.poseReason = "disabled", "pose_model_not_configured"
	} else {
		s.poseStatus, s.poseReason = "waiting_for_frames", "waiting_for_twenty_contiguous_raw_csi_frames"
	}
	return nil
}

func (s *Store) PoseInference(now time.Time) PoseInferenceSnapshot {
	if s == nil {
		return PoseInferenceSnapshot{Status: "disabled", Reason: "state_store_unavailable"}
	}
	s.poseMu.RLock()
	defer s.poseMu.RUnlock()
	snapshot := PoseInferenceSnapshot{
		Enabled: s.poseModel != nil, Status: s.poseStatus, Reason: s.poseReason,
		FramesProcessed: s.poseFramesProcessed,
	}
	if s.poseModel == nil {
		return snapshot
	}
	snapshot.Model = s.poseModel.Info()
	if s.poseNodeID != nil {
		value := *s.poseNodeID
		snapshot.SelectedNodeID = &value
	}
	if s.poseLatest != nil {
		copyOfEstimate := *s.poseLatest
		snapshot.Estimate = &copyOfEstimate
		age := now.Sub(copyOfEstimate.Timestamp)
		if age < 0 {
			age = 0
		}
		if age > 2*time.Second {
			snapshot.Status = "stale"
			snapshot.Reason = "latest_image_pose_is_older_than_two_seconds"
		}
	}
	return snapshot
}

// SetNodePositions installs the room geometry keyed by device identity. A
// missing position remains explicitly unavailable; it is never inferred from
// a node's index in a compacted fusion cohort.
func (s *Store) SetNodePositions(positions map[uint8][3]float64) error {
	if s == nil {
		return errors.New("nil state store")
	}
	copyPositions := make(map[uint8][3]float64, len(positions))
	for id, position := range positions {
		for _, coordinate := range position {
			if math.IsNaN(coordinate) || math.IsInf(coordinate, 0) {
				return errors.New("node position coordinates must be finite")
			}
		}
		copyPositions[id] = position
	}
	s.nodesMu.Lock()
	s.nodePositions = copyPositions
	for id, node := range s.nodes {
		node.mu.Lock()
		position, ok := copyPositions[id]
		node.position, node.positionAvailable = position, ok
		node.mu.Unlock()
	}
	s.nodesMu.Unlock()
	return nil
}

func (s *Store) SetSource(source string) {
	s.sourceMu.Lock()
	s.source = source
	s.activeSource = ""
	s.activeSeen = time.Time{}
	s.sourceMu.Unlock()
}

// SetCalibrationManager installs the room-calibration state used by CSI ingest
// and the HTTP calibration controls. Configure it before starting ingest.
func (s *Store) SetCalibrationManager(manager *calibration.Manager) {
	if s == nil {
		return
	}
	s.calibrationMu.Lock()
	s.calibration = manager
	s.calibrationMu.Unlock()
}

func (s *Store) calibrationManager() *calibration.Manager {
	if s == nil {
		return nil
	}
	s.calibrationMu.RLock()
	defer s.calibrationMu.RUnlock()
	return s.calibration
}

func (s *Store) ClearNodes() {
	s.nodesMu.Lock()
	s.nodes = make(map[uint8]*nodeRecord)
	s.nodesMu.Unlock()
	s.updateMu.Lock()
	s.roomDebouncedLevel = "absent"
	s.roomDebounceCandidate = "absent"
	s.roomDebounceSince = time.Time{}
	s.lastInputAt.Store(0)
	s.lastRefreshAt = time.Time{}
	s.latest.Store(nil)
	s.updateMu.Unlock()
	s.poseMu.Lock()
	s.poseWindows = make(map[uint8]*posemodel.WindowBuffer)
	s.poseLatest = nil
	if s.poseModel != nil {
		s.poseStatus, s.poseReason = "waiting_for_frames", "waiting_for_twenty_contiguous_raw_csi_frames"
	}
	s.poseMu.Unlock()
}

func (s *Store) IngestCSI(frame protocol.CSIFrame) {
	now := time.Now()
	frameSource := frame.Source
	if frameSource == "" {
		frameSource = "esp32"
	}
	s.sourceMu.Lock()
	if frameSource == "wifi" && s.activeSource == "esp32" && now.Sub(s.activeSeen) <= nodeFreshness {
		s.sourceMu.Unlock()
		return
	}
	s.activeSource, s.activeSeen = frameSource, now
	s.sourceMu.Unlock()
	node := s.getNode(frame.NodeID, now)
	if node == nil {
		return
	}
	if manager := s.calibrationManager(); manager != nil {
		manager.Observe(frame, now)
	}
	node.mu.Lock()
	sampleRateHz := node.observeFrameRate(now)
	node.meshTimestampUS = nil
	if frame.Flags&protocol.CSIFlagMeshSyncValid != 0 && node.latestSync != nil && node.latestSync.Flags.IsValid && now.Sub(node.syncSeen) <= meshSyncMaxAge {
		if timestampUS, ok := node.latestSync.MeshAlignedUSForSequence(frame.Sequence, sampleRateHz); ok {
			node.meshTimestampUS = &timestampUS
		}
	}
	observation := node.processor.Push(frame, now, sampleRateHz)
	if err := s.applyAdaptive(&observation); err != nil {
		s.adaptiveErrors.Add(1)
	}
	if frame.InputMode != "rssi_derived" {
		if err := s.applyRVFModel(&observation); err != nil {
			s.rvfInferenceErrors.Add(1)
		}
	}
	node.observation = &observation
	node.lastSeen = observation.Timestamp
	node.latestCSISequence = frame.Sequence
	node.hasCSI = true
	arrivalElapsedUS := now.Sub(s.started).Microseconds()
	if arrivalElapsedUS < 0 {
		arrivalElapsedUS = 0
	}
	node.arrivalTimestampUS = uint64(arrivalElapsedUS)
	node.rssiDBm = float64(frame.RSSIDBm)
	node.hasRSSI = true
	node.source = frameSource
	node.inputMode = frame.InputMode
	node.mu.Unlock()
	frame.Source = frameSource
	s.ingestPoseFrame(frame, now)
	s.frames.Add(1)
	s.lastInputAt.Store(now.UnixNano())
	s.publish()
}

func (s *Store) ingestPoseFrame(frame protocol.CSIFrame, now time.Time) {
	s.poseMu.Lock()
	defer s.poseMu.Unlock()
	if s.poseModel == nil {
		return
	}
	if s.poseNodeID != nil && frame.NodeID != *s.poseNodeID {
		return
	}
	if frame.Source != "esp32" && frame.Source != "simulate" && frame.Source != "synthetic" || frame.InputMode == "rssi_derived" {
		if window := s.poseWindows[frame.NodeID]; window != nil {
			window.Reset()
		}
		s.poseLatest = nil
		s.poseStatus, s.poseReason = "unavailable", "pose_inference_requires_raw_esp32_csi_not_rssi"
		return
	}
	if s.poseNodeID == nil {
		activeNodes := s.freshPoseNodes(now)
		if len(activeNodes) > 1 {
			s.poseWindows = make(map[uint8]*posemodel.WindowBuffer)
			s.poseLatest = nil
			s.poseStatus, s.poseReason = "unavailable", "pose_node_selection_required_for_multiple_sensors"
			return
		}
		if len(activeNodes) != 1 || activeNodes[0] != frame.NodeID {
			s.poseStatus, s.poseReason = "waiting_for_frames", "waiting_for_one_unambiguous_raw_csi_node"
			return
		}
	}
	feature, err := posemodel.FeatureFromCSI(frame)
	if err != nil {
		if window := s.poseWindows[frame.NodeID]; window != nil {
			window.Reset()
		}
		s.poseLatest = nil
		s.poseStatus, s.poseReason = "unavailable", "csi_frame_is_not_supported_by_pose_model"
		return
	}
	window := s.poseWindows[frame.NodeID]
	if window == nil {
		window = &posemodel.WindowBuffer{}
		s.poseWindows[frame.NodeID] = window
	}
	windowData, ready := window.Push(frame.Sequence, now, feature)
	if !ready {
		s.poseStatus, s.poseReason = "waiting_for_frames", "waiting_for_twenty_contiguous_raw_csi_frames"
		return
	}
	prediction, err := s.poseModel.Infer(windowData)
	if err != nil {
		s.poseLatest = nil
		s.poseStatus, s.poseReason = "unavailable", "pose_inference_failed"
		return
	}
	evidence := "unverified"
	if frame.Source == "simulate" || frame.Source == "synthetic" {
		evidence = "synthetic"
	}
	s.poseLatest = &PoseEstimate{Timestamp: now, NodeID: frame.NodeID, Source: frame.Source, Evidence: evidence, Prediction: prediction}
	s.poseFramesProcessed++
	s.poseStatus, s.poseReason = "experimental", "single_person_image_pose_only_no_person_detection_or_metric_validation"
}

func (s *Store) freshPoseNodes(now time.Time) []uint8 {
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
	active := make([]uint8, 0, len(pairs))
	for _, pair := range pairs {
		pair.node.mu.RLock()
		seen, source, mode, hasCSI := pair.node.lastSeen, pair.node.source, pair.node.inputMode, pair.node.hasCSI
		pair.node.mu.RUnlock()
		if !hasCSI || now.Sub(seen) > nodeFreshness || mode == "rssi_derived" {
			continue
		}
		if source == "esp32" || source == "simulate" || source == "synthetic" {
			active = append(active, pair.id)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i] < active[j] })
	return active
}

func (s *Store) SetAdaptiveModel(model *adaptive.Model) error {
	if model != nil {
		if err := model.Validate(); err != nil {
			return err
		}
		model = model.Clone()
	}
	s.adaptiveMu.Lock()
	s.adaptiveModel = model
	s.adaptiveMu.Unlock()
	return nil
}

func (s *Store) AdaptiveModel() (*adaptive.Model, bool) {
	if s == nil {
		return nil, false
	}
	s.adaptiveMu.RLock()
	defer s.adaptiveMu.RUnlock()
	if s.adaptiveModel == nil {
		return nil, false
	}
	return s.adaptiveModel.Clone(), true
}

func (s *Store) applyAdaptive(observation *signal.Observation) error {
	s.adaptiveMu.RLock()
	defer s.adaptiveMu.RUnlock()
	if s.adaptiveModel == nil {
		return nil
	}
	return s.adaptiveModel.Apply(observation)
}

// SetModelRegistry connects explicitly supported loaded RVF models to fresh
// CSI observations. Unknown RVF architectures stay inspectable and inert.
func (s *Store) SetModelRegistry(registry *model.Registry) {
	if s == nil {
		return
	}
	s.rvfModelsMu.Lock()
	s.rvfModels = registry
	s.rvfModelsMu.Unlock()
}

func (s *Store) applyRVFModel(observation *signal.Observation) error {
	if s == nil || observation == nil {
		return nil
	}
	s.rvfModelsMu.RLock()
	registry := s.rvfModels
	s.rvfModelsMu.RUnlock()
	if registry == nil {
		return nil
	}
	_, err := registry.ApplyObservation(observation)
	return err
}

// IngestVendorCSI stores a bounded vendor snapshot separately from the ESP32
// signal-processing path. It never converts vendor CSI summaries into people,
// pose, vitals, or metric positions.
func (s *Store) IngestVendorCSI(snapshot csivendor.Snapshot) {
	if s == nil || snapshot.EventType == "" || snapshot.Source == "" {
		return
	}
	now := time.Now()
	s.vendorCSIMu.Lock()
	s.vendorCSI[snapshot.EventType] = vendorCSIRecord{snapshot: cloneVendorCSISnapshot(snapshot), seenAt: now}
	if snapshot.EventType == "mediatek_csi" && snapshot.DeviceID != "" {
		if _, exists := s.mediatekCSIByDevice[snapshot.DeviceID]; !exists && len(s.mediatekCSIByDevice) >= maxMediaTekCSIDevices {
			var oldestID string
			var oldest time.Time
			for deviceID, record := range s.mediatekCSIByDevice {
				if oldestID == "" || record.seenAt.Before(oldest) {
					oldestID, oldest = deviceID, record.seenAt
				}
			}
			delete(s.mediatekCSIByDevice, oldestID)
			delete(s.mediatekCSIRingByDevice, oldestID)
			s.mediatekActivity.Remove(oldestID)
		}
		s.mediatekCSIByDevice[snapshot.DeviceID] = vendorCSIRecord{snapshot: cloneVendorCSISnapshot(snapshot), seenAt: now}
		if snapshot.Frame != nil {
			ring := s.mediatekCSIRingByDevice[snapshot.DeviceID]
			if ring == nil {
				ring = &csivendor.Ring{}
				s.mediatekCSIRingByDevice[snapshot.DeviceID] = ring
			}
			ring.Push(*snapshot.Frame)
			fast, frames := ring.FastActivity(activity.FastWindow)
			s.mediatekActivity.Observe(snapshot.DeviceID, now, fast, frames)
		}
	}
	s.vendorCSIMu.Unlock()
	s.sourceMu.Lock()
	if s.activeSource != "esp32" || now.Sub(s.activeSeen) > nodeFreshness {
		s.source = snapshot.Source
		s.activeSource = snapshot.Source
		s.activeSeen = now
	}
	s.sourceMu.Unlock()
	s.frames.Add(1)
}

func (s *Store) MediaTekActivityHistory(now time.Time, seconds uint64) []activity.HistorySample {
	if s == nil || s.mediatekActivity == nil {
		return []activity.HistorySample{}
	}
	return s.mediatekActivity.History(now, seconds)
}

func (s *Store) SeedMediaTekActivityFloors(floors map[string]float64) {
	if s != nil && s.mediatekActivity != nil {
		s.mediatekActivity.SeedFloors(floors)
	}
}

func (s *Store) MediaTekActivityFloors() map[string]float64 {
	if s == nil || s.mediatekActivity == nil {
		return map[string]float64{}
	}
	return s.mediatekActivity.Floors()
}

// MediaTekCSIDevices returns fresh per-device bridge snapshots ordered by
// device ID. Stale entries remain bounded in memory but disappear from the API.
func (s *Store) MediaTekCSIDevices(now time.Time) []MediaTekDevice {
	if s == nil {
		return []MediaTekDevice{}
	}
	s.vendorCSIMu.RLock()
	devices := make([]MediaTekDevice, 0, len(s.mediatekCSIByDevice))
	for _, record := range s.mediatekCSIByDevice {
		age := now.Sub(record.seenAt)
		if age < 0 {
			age = 0
		}
		if age > mediaTekDeviceFreshness {
			continue
		}
		devices = append(devices, MediaTekDevice{Snapshot: cloneVendorCSISnapshot(record.snapshot), AgeMS: uint64(age.Milliseconds())})
	}
	s.vendorCSIMu.RUnlock()
	sort.Slice(devices, func(i, j int) bool { return devices[i].Snapshot.DeviceID < devices[j].Snapshot.DeviceID })
	return devices
}

type MediaTekFrameSet struct {
	SubcarrierCount uint16
	Chains          []csivendor.ChainPosition
	Frames          []csivendor.CSIFrame
}

func (s *Store) MediaTekCSIFrames(deviceID string, n int) (MediaTekFrameSet, bool) {
	if s == nil {
		return MediaTekFrameSet{}, false
	}
	s.vendorCSIMu.RLock()
	ring, ok := s.mediatekCSIRingByDevice[deviceID]
	if !ok || ring.Len() == 0 {
		s.vendorCSIMu.RUnlock()
		return MediaTekFrameSet{}, false
	}
	frames := ring.Tail(n)
	result := MediaTekFrameSet{
		SubcarrierCount: ring.SubcarrierCount(), Frames: frames,
		Chains: csivendor.ChainUnion(frames),
	}
	s.vendorCSIMu.RUnlock()
	return result, true
}

func (s *Store) MediaTekCSISummary(deviceID string) (csivendor.RingSummary, bool) {
	if s == nil {
		return csivendor.RingSummary{}, false
	}
	s.vendorCSIMu.RLock()
	ring, ok := s.mediatekCSIRingByDevice[deviceID]
	if !ok || ring.Len() == 0 {
		s.vendorCSIMu.RUnlock()
		return csivendor.RingSummary{}, false
	}
	summary := ring.Summary()
	s.vendorCSIMu.RUnlock()
	return summary, true
}

func (s *Store) LatestVendorCSI(eventType string) (csivendor.Snapshot, bool) {
	if s == nil {
		return csivendor.Snapshot{}, false
	}
	s.vendorCSIMu.RLock()
	record, ok := s.vendorCSI[eventType]
	s.vendorCSIMu.RUnlock()
	if !ok {
		return csivendor.Snapshot{}, false
	}
	return cloneVendorCSISnapshot(record.snapshot), true
}

// IngestRadar retains only the latest bounded aggregate. Radar data stays
// separate from the Wi-Fi CSI sensing path and never creates person outputs.
func (s *Store) IngestRadar(snapshot radar.Snapshot) {
	if s == nil || snapshot.EventType != "realtek_radar" {
		return
	}
	copyOfSnapshot := cloneRadarSnapshot(snapshot)
	s.vendorCSIMu.Lock()
	s.latestRadar = &copyOfSnapshot
	s.vendorCSIMu.Unlock()
}

func (s *Store) LatestRadar() (radar.Snapshot, bool) {
	if s == nil {
		return radar.Snapshot{}, false
	}
	s.vendorCSIMu.RLock()
	defer s.vendorCSIMu.RUnlock()
	if s.latestRadar == nil {
		return radar.Snapshot{}, false
	}
	return cloneRadarSnapshot(*s.latestRadar), true
}

func cloneRadarSnapshot(snapshot radar.Snapshot) radar.Snapshot {
	clone := snapshot
	if snapshot.PeakRangeM != nil {
		v := *snapshot.PeakRangeM
		clone.PeakRangeM = &v
	}
	if snapshot.PeakPower != nil {
		v := *snapshot.PeakPower
		clone.PeakPower = &v
	}
	if snapshot.MeanCFRAmplitude != nil {
		v := *snapshot.MeanCFRAmplitude
		clone.MeanCFRAmplitude = &v
	}
	return clone
}

func cloneVendorCSISnapshot(snapshot csivendor.Snapshot) csivendor.Snapshot {
	clone := snapshot
	clone.RSSIDBm = append([]int8(nil), snapshot.RSSIDBm...)
	if snapshot.Frame != nil {
		frame := snapshot.Frame.Clone()
		clone.Frame = &frame
	}
	if snapshot.MeanAmplitude != nil {
		value := *snapshot.MeanAmplitude
		clone.MeanAmplitude = &value
	}
	if snapshot.PeakAmplitude != nil {
		value := *snapshot.PeakAmplitude
		clone.PeakAmplitude = &value
	}
	return clone
}

func (s *Store) IngestSync(packet protocol.SyncPacket) {
	now := time.Now()
	node := s.getNode(packet.NodeID, now)
	if node == nil {
		return
	}
	node.mu.Lock()
	copyPacket := packet
	node.latestSync = &copyPacket
	node.syncSeen = now
	node.mu.Unlock()
	s.syncPackets.Add(1)
}

func (s *Store) IngestVitals(packet protocol.VitalsPacket) {
	now := time.Now()
	s.setActiveSource("esp32", now)
	node := s.getNode(packet.NodeID, now)
	if node == nil {
		return
	}
	node.mu.Lock()
	copyPacket := packet
	node.edgeVitals = &copyPacket
	node.rssiDBm = float64(packet.RSSIDBm)
	node.hasRSSI = true
	node.vitalsSeen = now
	node.lastSeen = node.vitalsSeen
	node.mu.Unlock()
	s.vitals.Add(1)
	if !packet.CountValid {
		s.badCounts.Add(1)
	}
	s.lastInputAt.Store(now.UnixNano())
	s.publish()
}

// IngestEdgeFusedVitals preserves the firmware's fused telemetry WebSocket
// event while using only the currently trusted occupancy fields in shared
// sensing state. Raw heart and breathing values are never copied into the
// public sensing update.
func (s *Store) IngestEdgeFusedVitals(packet protocol.EdgeFusedVitalsPacket) {
	s.IngestVitals(packet.Vitals())
	if node := s.getNode(packet.NodeID, time.Now()); node != nil {
		node.mu.Lock()
		node.fusionConfidence = packet.FusionConfidence
		node.mu.Unlock()
	}
	event := struct {
		Type                 string  `json:"type"`
		NodeID               uint8   `json:"node_id"`
		Persons              uint8   `json:"n_persons"`
		PersonCountValid     bool    `json:"person_count_valid"`
		PersonCountAuthority string  `json:"person_count_authority"`
		MeasurementAuthority string  `json:"measurement_authority"`
		FusionConfidence     uint8   `json:"fusion_confidence"`
		BiometricAuthority   string  `json:"biometric_authority"`
		BiometricsAuthorized bool    `json:"biometrics_authorized"`
		MmwaveTargets        uint8   `json:"mmwave_targets"`
		MmwaveConfidence     uint8   `json:"mmwave_confidence"`
		MmwaveType           uint8   `json:"mmwave_type"`
		MotionEnergy         float64 `json:"motion_energy"`
		PresenceScore        float64 `json:"presence_score"`
		TimestampMS          uint32  `json:"timestamp_ms"`
	}{
		Type: "edge_fused_vitals", NodeID: packet.NodeID,
		Persons: packet.Persons, PersonCountValid: packet.PersonCountValid,
		PersonCountAuthority: "unverified", MeasurementAuthority: "unverified",
		FusionConfidence:   packet.FusionConfidence,
		BiometricAuthority: "abstained", BiometricsAuthorized: false,
		MmwaveTargets: packet.MmwaveTargets, MmwaveConfidence: packet.MmwaveConfidence,
		MmwaveType: packet.MmwaveType, MotionEnergy: packet.MotionEnergy,
		PresenceScore: packet.PresenceScore, TimestampMS: packet.TimestampMS,
	}
	if encoded, err := json.Marshal(event); err == nil {
		s.hub.Publish(encoded)
	}
}

// FreshPersonCountSum returns only valid per-node count packets observed within
// maxAge. It is used to tune multi-node deduplication and never treats stale
// telemetry as current ground truth input.
func (s *Store) FreshPersonCountSum(now time.Time, maxAge time.Duration) (uint64, int) {
	if s == nil || maxAge <= 0 {
		return 0, 0
	}
	s.nodesMu.RLock()
	nodes := make([]*nodeRecord, 0, len(s.nodes))
	for _, node := range s.nodes {
		nodes = append(nodes, node)
	}
	s.nodesMu.RUnlock()
	var total uint64
	contributors := 0
	for _, node := range nodes {
		node.mu.RLock()
		fresh := !node.vitalsSeen.IsZero() && now.Sub(node.vitalsSeen) >= 0 && now.Sub(node.vitalsSeen) <= maxAge
		if fresh && node.edgeVitals != nil && node.edgeVitals.CountValid {
			total += uint64(node.edgeVitals.Persons)
			contributors++
		}
		node.mu.RUnlock()
	}
	return total, contributors
}

// LatestFusionConfidence returns a normalized confidence from a fresh edge
// fusion packet. It is an input to the trust gate, not sufficient authority.
func (s *Store) LatestFusionConfidence(now time.Time) *float64 {
	if s == nil {
		return nil
	}
	s.nodesMu.RLock()
	nodes := make([]*nodeRecord, 0, len(s.nodes))
	for _, node := range s.nodes {
		nodes = append(nodes, node)
	}
	s.nodesMu.RUnlock()
	var best uint8
	found := false
	for _, node := range nodes {
		node.mu.RLock()
		if node.edgeVitals != nil && !node.vitalsSeen.IsZero() && now.Sub(node.vitalsSeen) >= 0 && now.Sub(node.vitalsSeen) <= latestStale && node.fusionConfidence > best {
			best, found = node.fusionConfidence, true
		}
		node.mu.RUnlock()
	}
	if !found {
		return nil
	}
	confidence := float64(best) / 255
	return &confidence
}

func (s *Store) IngestWASMOutput(packet protocol.WASMOutputPacket) {
	if s == nil {
		return
	}
	copyPacket := packet
	copyPacket.Events = append([]protocol.WASMEvent(nil), packet.Events...)
	s.wasmMu.Lock()
	s.wasmLatest = &copyPacket
	s.wasmMu.Unlock()
	event := struct {
		Type     string               `json:"type"`
		NodeID   uint8                `json:"node_id"`
		ModuleID uint8                `json:"module_id"`
		Events   []protocol.WASMEvent `json:"events"`
	}{Type: "wasm_event", NodeID: copyPacket.NodeID, ModuleID: copyPacket.ModuleID, Events: copyPacket.Events}
	if encoded, err := json.Marshal(event); err == nil {
		s.hub.Publish(encoded)
	}
}

func (s *Store) LatestWASMOutput() (protocol.WASMOutputPacket, bool) {
	if s == nil {
		return protocol.WASMOutputPacket{}, false
	}
	s.wasmMu.RLock()
	defer s.wasmMu.RUnlock()
	if s.wasmLatest == nil {
		return protocol.WASMOutputPacket{}, false
	}
	packet := *s.wasmLatest
	packet.Events = append([]protocol.WASMEvent(nil), packet.Events...)
	return packet, true
}

func (s *Store) IngestFeatureState(packet protocol.FeatureStatePacket) {
	now := time.Now()
	s.setActiveSource("esp32", now)
	node := s.getNode(packet.NodeID, now)
	if node == nil {
		return
	}
	node.mu.Lock()
	copyPacket := packet
	node.featureState = &copyPacket
	node.featureSeen = now
	node.lastSeen = node.featureSeen
	node.mu.Unlock()
	s.frames.Add(1)
	s.lastInputAt.Store(now.UnixNano())
	s.publish()
}

func (s *Store) getNode(id uint8, now time.Time) *nodeRecord {
	s.nodesMu.Lock()
	defer s.nodesMu.Unlock()
	if node, ok := s.nodes[id]; ok {
		return node
	}
	if len(s.nodes) >= maxTrackedNodes {
		var oldestID uint8
		oldest := now
		found := false
		for nodeID, candidate := range s.nodes {
			candidate.mu.RLock()
			seen := candidate.lastSeen
			candidate.mu.RUnlock()
			if !found || seen.Before(oldest) {
				oldestID, oldest, found = nodeID, seen, true
			}
		}
		if !found || now.Sub(oldest) < nodeFreshness {
			return nil
		}
		delete(s.nodes, oldestID)
	}
	node := &nodeRecord{processor: signal.NewProcessor(), csiFPSEMA: defaultSampleRateHz}
	if position, ok := s.nodePositions[id]; ok {
		node.position, node.positionAvailable = position, true
	}
	s.nodes[id] = node
	return node
}

func (s *Store) nodeViews(now time.Time) []nodeView {
	s.nodesMu.RLock()
	pairs := make([]struct {
		id   uint8
		node *nodeRecord
	}, 0, len(s.nodes))
	for id, node := range s.nodes {
		pairs = append(pairs, struct {
			id   uint8
			node *nodeRecord
		}{id, node})
	}
	s.nodesMu.RUnlock()
	views := make([]nodeView, 0, len(pairs))
	for _, pair := range pairs {
		pair.node.mu.RLock()
		view := nodeView{
			id: pair.id, lastSeen: pair.node.lastSeen, position: pair.node.position,
			positionAvailable: pair.node.positionAvailable, sequence: pair.node.latestCSISequence,
			arrivalTimestampUS: pair.node.arrivalTimestampUS, source: pair.node.source,
			featureSeen: pair.node.featureSeen, vitalsSeen: pair.node.vitalsSeen,
			inputMode: pair.node.inputMode,
		}
		if pair.node.meshTimestampUS != nil {
			value := *pair.node.meshTimestampUS
			view.meshTimestampUS = &value
		}
		if pair.node.latestSync != nil {
			packet := *pair.node.latestSync
			view.sync = makeSyncSnapshot(packet, pair.node.syncSeen, pair.node.csiFPSEMA, pair.node.csiFPSSamples, now)
		}
		if pair.node.observation != nil && now.Sub(pair.node.observation.Timestamp) <= latestStale {
			value := *pair.node.observation
			view.observation = &value
		}
		if pair.node.featureState != nil && now.Sub(pair.node.featureSeen) <= latestStale {
			value := *pair.node.featureState
			view.featureState = &value
		}
		if pair.node.edgeVitals != nil && now.Sub(pair.node.vitalsSeen) <= latestStale {
			value := *pair.node.edgeVitals
			view.edgeVitals = &value
		}
		pair.node.mu.RUnlock()
		if !view.lastSeen.IsZero() && now.Sub(view.lastSeen) <= latestStale {
			views = append(views, view)
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].id < views[j].id })
	return views
}

func (s *Store) publish() {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	now := time.Now()
	s.updateRate.Observe(now)
	s.publishLocked(now)
}

// Refresh publishes a stale/freshness transition or completes a pending room
// debounce when no sensor packet has arrived to trigger a normal publish.
func (s *Store) Refresh() {
	s.refreshAt(time.Now())
}

func (s *Store) refreshAt(now time.Time) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	lastInput := s.lastInputAt.Load()
	if lastInput == 0 {
		return
	}
	inputAge := now.Sub(time.Unix(0, lastInput))
	if inputAge < 0 {
		inputAge = 0
	}
	debounceDue := !s.roomDebounceSince.IsZero() && now.Sub(s.roomDebounceSince) >= roomDebounceDuration
	staleRefreshDue := inputAge >= nodeFreshness && (s.lastRefreshAt.IsZero() || now.Sub(s.lastRefreshAt) >= time.Second)
	if !debounceDue && !staleRefreshDue {
		return
	}
	s.publishLocked(now)
	s.lastRefreshAt = now
}

func (s *Store) publishLocked(now time.Time) {
	views := s.nodeViews(now)
	update := &SensingUpdate{
		Type: "sensing_update", Timestamp: float64(now.UnixNano()) / 1e9,
		Source: s.effectiveSource(len(views) > 0), Tick: s.tick.Add(1),
		MeasurementAuthority: "unverified", CalibratedEvidenceAuthorized: false,
		NumericVitalsAuthorized: false,
		Nodes:                   make([]NodeInfo, 0, len(views)), NodeFeatures: make([]NodeFeatureInfo, 0, len(views)),
		SpatialFusion:  multistatic.EmptySnapshot(),
		Classification: signal.Classification{MotionLevel: "absent"},
		SignalField:    signal.SignalField{GridSize: [3]int{0, 0, 0}, Values: []float64{}},
	}
	if lastInput := s.lastInputAt.Load(); lastInput != 0 {
		update.observedAt = time.Unix(0, lastInput)
	} else {
		for _, node := range views {
			if node.lastSeen.After(update.observedAt) {
				update.observedAt = node.lastSeen
			}
		}
	}
	var newestObservation *signal.Observation
	spatialFrames := make([]multistatic.Frame, 0, len(views))
	var personCount uint8
	var personCountSum uint64
	hasPersonCount := false
	for _, node := range views {
		info := NodeInfo{NodeID: node.id, Position: node.position, PositionAvailable: node.positionAvailable, Amplitude: []float64{}, Sync: node.sync, MeshTimestampUS: node.meshTimestampUS}
		features := NodeFeatureInfo{NodeID: node.id, LastSeenMS: uint64(now.Sub(node.lastSeen).Milliseconds())}
		if node.observation != nil {
			obs := node.observation
			info.RSSIDBm = float64(obs.RSSIDBm)
			info.Amplitude = append([]float64(nil), obs.Amplitude...)
			info.SubcarrierCount = obs.Subcarriers
			features.Features, features.MotionScore = obs.Features, obs.Motion
			if newestObservation == nil || obs.Timestamp.After(newestObservation.Timestamp) {
				newestObservation = obs
				update.InputMode = node.inputMode
			}
			phaseReference := multistatic.PhaseUnavailable
			if node.source == "esp32" && len(obs.Phase) > 0 && len(obs.Phase) == len(obs.Amplitude) {
				phaseReference = multistatic.PhaseNodeLocal
			}
			spatialFrames = append(spatialFrames, multistatic.Frame{
				NodeID: node.id, Sequence: node.sequence, Subcarriers: obs.Subcarriers,
				Amplitude: append([]float64(nil), obs.Amplitude...), Phase: append([]float64(nil), obs.Phase...),
				PhaseReference: phaseReference, MeshTimestampUS: node.meshTimestampUS,
				ArrivalTimestampUS: node.arrivalTimestampUS, SeenAt: obs.Timestamp,
				Position: node.position, PositionAvailable: node.positionAvailable,
				Source: node.source, InputMode: node.inputMode,
			})
		}
		if node.featureState != nil {
			fs := node.featureState
			features.MotionScore = fs.MotionScore
			features.Classification = featureStateClassification(*fs)
			if newestObservation == nil || node.featureSeen.After(newestObservation.Timestamp) {
				copy := signal.Observation{Timestamp: node.featureSeen, NodeID: node.id, Class: features.Classification, Motion: fs.MotionScore, Quality: fs.NodeCoherence}
				newestObservation = &copy
				update.InputMode = "edge_feature_state"
			}
		}
		if node.edgeVitals != nil {
			e := node.edgeVitals
			if e.CountValid {
				if !hasPersonCount || e.Persons > personCount {
					personCount = e.Persons
				}
				personCountSum += uint64(e.Persons)
				hasPersonCount = true
			}
		}
		features.Classification = nodeFeatureClassification(node)
		update.Nodes = append(update.Nodes, info)
		update.NodeFeatures = append(update.NodeFeatures, features)
	}
	update.SpatialFusion = multistatic.FuseLatest(spatialFrames, now, multistatic.DefaultOptions())
	if newestObservation != nil {
		update.Features = newestObservation.Features
		update.SignalField = newestObservation.Field
		update.SignalQualityScore = floatPointer(newestObservation.Quality)
	}
	roomNodes := make([]inference.Node, 0, len(views))
	for _, node := range views {
		classification := nodeFeatureClassification(node)
		age := uint64(max(0, now.Sub(node.lastSeen).Milliseconds()))
		roomNodes = append(roomNodes, inference.Node{Classification: classification.MotionLevel, Confidence: nodeFeatureConfidence(node), AgeMS: &age})
	}
	room := inference.FuseRoom(roomNodes, uint64(latestStale.Milliseconds()))
	update.Classification = s.debounceRoomClassification(room, now)
	if update.Classification.Presence && hasPersonCount {
		factor := calibration.DefaultDedupFactor
		if manager := s.calibrationManager(); manager != nil {
			factor = manager.DedupFactor()
		}
		count := max(personCount, deduplicatedPersonCount(personCountSum, factor))
		if count == 0 {
			count = 1
		}
		update.EstimatedPersons = &count
	}
	s.latest.Store(update)
	if encoded, err := json.Marshal(update); err == nil {
		s.hub.Publish(encoded)
	}
}

func deduplicatedPersonCount(rawSum uint64, factor float64) uint8 {
	if rawSum == 0 {
		return 0
	}
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor < 1 || factor > 10 {
		factor = calibration.DefaultDedupFactor
	}
	count := math.Ceil(float64(rawSum) / factor)
	if count > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(count)
}

// debounceRoomClassification ports the Rust room-level hysteresis from
// inference.rs: discrete room state changes only after the new winning class
// remains stable for 1.5 seconds. Confidence remains the current fused value.
// publish holds updateMu while calling this method.
func (s *Store) debounceRoomClassification(raw inference.Room, now time.Time) signal.Classification {
	candidate := raw.Classification
	if candidate == "" || candidate == "unavailable" {
		candidate = "absent"
	}
	if s.roomDebouncedLevel == "" {
		s.roomDebouncedLevel = "absent"
	}
	switch {
	case candidate == s.roomDebouncedLevel:
		s.roomDebounceCandidate = candidate
		s.roomDebounceSince = time.Time{}
	case candidate == s.roomDebounceCandidate:
		if s.roomDebounceSince.IsZero() {
			s.roomDebounceSince = now
		}
		if now.Sub(s.roomDebounceSince) >= roomDebounceDuration {
			s.roomDebouncedLevel = candidate
			s.roomDebounceSince = time.Time{}
		}
	default:
		s.roomDebounceCandidate = candidate
		s.roomDebounceSince = now
	}
	return signal.Classification{
		MotionLevel: s.roomDebouncedLevel,
		Presence:    s.roomDebouncedLevel != "absent",
		Confidence:  clamp01(raw.Confidence),
	}
}

func (s *Store) Latest(now time.Time) (SensingUpdate, string, time.Duration) {
	update := s.latest.Load()
	if update == nil {
		return SensingUpdate{}, "no data yet", 0
	}
	if update.observedAt.IsZero() {
		return SensingUpdate{}, "no data yet", 0
	}
	age := now.Sub(update.observedAt)
	if age < 0 {
		// The public timestamp is Unix seconds in float64, which can round a
		// freshly generated event a few hundred nanoseconds into the future.
		age = 0
	}
	if age > latestStale {
		return SensingUpdate{Source: s.effectiveSource(false)}, "stale", age
	}
	return *update, "fresh", age
}

func (s *Store) Health() map[string]any {
	now := time.Now()
	views := s.nodeViews(now)
	source := s.effectiveSource(len(views) > 0)
	vendorCSIOnline := s.hasFreshVendorCSI(now)
	sourceState := "disconnected"
	if source == "simulate" || strings.HasSuffix(source, ":simulated") {
		sourceState = "synthetic"
	} else if len(views) > 0 || vendorCSIOnline {
		sourceState = "connected"
	}
	lastAge := any(nil)
	if len(views) > 0 {
		newest := views[0].lastSeen
		for _, view := range views[1:] {
			if view.lastSeen.After(newest) {
				newest = view.lastSeen
			}
		}
		lastAge = uint64(now.Sub(newest).Milliseconds())
	} else if _, seen, ok := s.latestVendorCSISource(); ok {
		age := now.Sub(seen)
		if age < 0 {
			age = 0
		}
		lastAge = uint64(age.Milliseconds())
	}
	return map[string]any{
		"status": "ready", "source": source, "source_state": sourceState,
		"waiting_for_frames": len(views) == 0 && !vendorCSIOnline, "last_frame_age_ms": lastAge,
		"uptime_seconds": uint64(now.Sub(s.started).Seconds()),
		"nodes":          len(views), "tick": s.tick.Load(), "frames_received": s.frames.Load(), "vitals_packets": s.vitals.Load(),
		"invalid_person_counts": s.badCounts.Load(), "sync_packets": s.syncPackets.Load(),
		"adaptive_classification_errors": s.adaptiveErrors.Load(),
		"rvf_model_inference_errors":     s.rvfInferenceErrors.Load(),
	}
}

// MeshSync returns the most recently received sync packet for each tracked
// node, keyed by decimal node ID like the Rust REST response. Old packets are
// retained with an increasing staleness value so clients can show decay.
func (s *Store) MeshSync(now time.Time) map[string]SyncSnapshot {
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
	snapshots := make(map[string]SyncSnapshot)
	for _, pair := range pairs {
		pair.node.mu.RLock()
		if pair.node.latestSync != nil {
			snapshots[strconv.Itoa(int(pair.id))] = *makeSyncSnapshot(*pair.node.latestSync, pair.node.syncSeen, pair.node.csiFPSEMA, pair.node.csiFPSSamples, now)
		}
		pair.node.mu.RUnlock()
	}
	return snapshots
}

func (s *Store) NodeCount() int {
	s.nodesMu.RLock()
	defer s.nodesMu.RUnlock()
	return len(s.nodes)
}

// NodeSync returns a per-node mesh snapshot and distinguishes an unknown node
// from a known node that has not emitted a sync packet yet.
func (s *Store) NodeSync(id uint8, now time.Time) (SyncSnapshot, string) {
	s.nodesMu.RLock()
	node, ok := s.nodes[id]
	s.nodesMu.RUnlock()
	if !ok {
		return SyncSnapshot{}, "unknown_node"
	}
	node.mu.RLock()
	defer node.mu.RUnlock()
	if node.latestSync == nil {
		return SyncSnapshot{}, "no_sync"
	}
	return *makeSyncSnapshot(*node.latestSync, node.syncSeen, node.csiFPSEMA, node.csiFPSSamples, now), "ok"
}

func (n *nodeRecord) observeFrameRate(now time.Time) float64 {
	if !n.lastFrameArrival.IsZero() {
		delta := now.Sub(n.lastFrameArrival).Seconds()
		if delta > 0 && delta < 1 && n.csiFPSEMA > 0 {
			previousDelta := 1 / n.csiFPSEMA
			deltaEMA := previousDelta + (delta-previousDelta)*sampleRateEMAAlpha
			if deltaEMA > 0 {
				n.csiFPSEMA = 1 / deltaEMA
				n.csiFPSSamples++
			}
		}
	}
	n.lastFrameArrival = now
	if n.csiFPSSamples < 5 {
		return defaultSampleRateHz
	}
	return min(max(n.csiFPSEMA, 1), maxPhysicalSampleRateHz)
}

func makeSyncSnapshot(packet protocol.SyncPacket, seen time.Time, fpsEMA float64, fpsSamples uint32, now time.Time) *SyncSnapshot {
	if fpsEMA <= 0 {
		fpsEMA = defaultSampleRateHz
	}
	if fpsSamples < 5 {
		fpsEMA = defaultSampleRateHz
	} else {
		fpsEMA = min(max(fpsEMA, 1), maxPhysicalSampleRateHz)
	}
	var staleMS *uint64
	if !seen.IsZero() {
		age := now.Sub(seen)
		if age < 0 {
			age = 0
		}
		value := uint64(age.Milliseconds())
		staleMS = &value
	}
	return &SyncSnapshot{
		OffsetUS: packet.OffsetUS(), IsLeader: packet.Flags.IsLeader, IsValid: packet.Flags.IsValid,
		Smoothed: packet.Flags.SmoothedUsed, Sequence: packet.Sequence,
		CSIFPSEMA: fpsEMA, CSIFPSSamples: fpsSamples, StalenessMS: staleMS,
	}
}

func (s *Store) SnapshotNodes() []NodeInfo {
	update := s.latest.Load()
	if update == nil {
		return []NodeInfo{}
	}
	return append([]NodeInfo(nil), update.Nodes...)
}

func (s *Store) NodeStatuses(now time.Time) []NodeStatus {
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

	statuses := make([]NodeStatus, 0, len(pairs))
	for _, pair := range pairs {
		pair.node.mu.RLock()
		status := NodeStatus{
			NodeID: pair.id, Status: "stale", LastSeenMS: 999999, CSIStatus: "stale",
			RSSIDBm: -90, MotionLevel: "absent", PersonCount: 0,
			PositionConfigured: pair.node.positionAvailable,
		}
		if pair.node.hasRSSI {
			status.RSSIDBm = pair.node.rssiDBm
		}
		if pair.node.observation != nil {
			ageDuration := now.Sub(pair.node.observation.Timestamp)
			if ageDuration < 0 {
				ageDuration = 0
			}
			age := durationMillis(ageDuration)
			status.LastSeenMS = age
			if age <= uint64(nodeFreshness.Milliseconds()) {
				status.Status = "active"
			}
			if ageDuration < nodeFreshness {
				status.CSIStatus = "active"
			}
			lastSeen := age
			status.CSILastSeenMS = &lastSeen
		}
		if pair.node.hasCSI {
			sequence := pair.node.latestCSISequence
			status.CSISequence = &sequence
		}
		if pair.node.edgeVitals != nil {
			valid := pair.node.edgeVitals.CountValid
			status.PersonCountValid = &valid
			if valid {
				status.PersonCount = pair.node.edgeVitals.Persons
			}
		}
		status.MotionLevel = nodeFeatureClassification(nodeView{
			observation: pair.node.observation, featureState: pair.node.featureState, edgeVitals: pair.node.edgeVitals,
		}).MotionLevel
		pair.node.mu.RUnlock()
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].NodeID < statuses[j].NodeID })
	return statuses
}

func durationMillis(duration time.Duration) uint64 {
	if duration < 0 {
		return 0
	}
	return uint64(duration.Milliseconds())
}

func (s *Store) Hub() *ws.Hub          { return s.hub }
func (s *Store) Uptime() time.Duration { return time.Since(s.started) }

func (s *Store) effectiveSource(hasFresh bool) string {
	s.sourceMu.RLock()
	source := s.source
	activeSource := s.activeSource
	activeSeen := s.activeSeen
	s.sourceMu.RUnlock()
	if source == "simulate" || source == "simulated" {
		return "simulate"
	}
	now := time.Now()
	if vendorSource, seen, ok := s.latestVendorCSISource(); ok && now.Sub(seen) <= nodeFreshness {
		if activeSource != "esp32" && (activeSource == vendorSource || activeSource == "" || now.Sub(activeSeen) > nodeFreshness || seen.After(activeSeen)) {
			return vendorSource
		}
	}
	if activeSource != "" {
		if hasFresh {
			return activeSource
		}
		return activeSource + ":offline"
	}
	if hasFresh {
		return source
	}
	if source == "wifi" {
		return "wifi:offline"
	}
	if source == "esp32" {
		return "esp32:offline"
	}
	return source + ":offline"
}

func (s *Store) latestVendorCSISource() (string, time.Time, bool) {
	s.vendorCSIMu.RLock()
	defer s.vendorCSIMu.RUnlock()
	var latest vendorCSIRecord
	found := false
	for _, record := range s.vendorCSI {
		if !found || record.seenAt.After(latest.seenAt) {
			latest, found = record, true
		}
	}
	if !found {
		return "", time.Time{}, false
	}
	return latest.snapshot.Source, latest.seenAt, true
}

func (s *Store) hasFreshVendorCSI(now time.Time) bool {
	_, seen, ok := s.latestVendorCSISource()
	return ok && now.Sub(seen) <= nodeFreshness
}

func (s *Store) setActiveSource(source string, seen time.Time) {
	s.sourceMu.Lock()
	s.activeSource, s.activeSeen = source, seen
	s.sourceMu.Unlock()
}

func nodeFeatureClassification(node nodeView) signal.Classification {
	if node.edgeVitals != nil {
		return vitalsClassification(*node.edgeVitals)
	}
	if node.featureState != nil {
		return featureStateClassification(*node.featureState)
	}
	if node.observation != nil {
		return node.observation.Class
	}
	return signal.Classification{MotionLevel: "absent"}
}

func nodeFeatureConfidence(node nodeView) float64 {
	if node.edgeVitals != nil {
		return clamp01(node.edgeVitals.PresenceScore)
	}
	if node.featureState != nil {
		return clamp01(node.featureState.PresenceScore)
	}
	if node.observation != nil {
		return node.observation.Class.Confidence
	}
	return 0
}

func motionLevel(score float64) string {
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

func featureStateClassification(packet protocol.FeatureStatePacket) signal.Classification {
	level := motionLevel(packet.MotionScore)
	presenceValid := packet.QualityFlags&1 != 0
	if level == "absent" && presenceValid && packet.PresenceScore >= 0.5 {
		level = "present_still"
	}
	return signal.Classification{MotionLevel: level, Presence: level != "absent", Confidence: clamp01(packet.PresenceScore)}
}

func vitalsClassification(packet protocol.VitalsPacket) signal.Classification {
	level := "absent"
	if packet.Motion {
		level = "present_moving"
	} else if packet.Presence {
		level = "present_still"
	}
	return signal.Classification{MotionLevel: level, Presence: level != "absent", Confidence: clamp01(packet.PresenceScore)}
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
func floatPointer(value float64) *float64 { return &value }
