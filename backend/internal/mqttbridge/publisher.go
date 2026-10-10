// Package mqttbridge publishes the Go backend's deliberately small, safe
// sensing projection through MQTT and Home Assistant discovery.
package mqttbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/ruvnet/ruview/backend/internal/state"
	stream "github.com/ruvnet/ruview/backend/internal/ws"
)

const (
	maxKnownNodes         = 64
	maxOutboundMessages   = 512
	connectionTimeout     = 5 * time.Second
	publishAckTimeout     = 3 * time.Second
	availabilityHeartbeat = 30 * time.Second
	maxStateFrameBytes    = 1 << 20
)

var ErrUnavailable = errors.New("MQTT integration is unavailable")

type NodeSnapshot struct {
	ID       string
	Presence bool
	Motion   bool
	RSSIDBm  *float64
	At       time.Time
}

type Availability struct {
	Topic               string `json:"topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`
}

type DiscoveryConfig struct {
	Name              string         `json:"name"`
	UniqueID          string         `json:"unique_id"`
	ObjectID          string         `json:"object_id"`
	StateTopic        string         `json:"state_topic"`
	Availability      []Availability `json:"availability"`
	AvailabilityMode  string         `json:"availability_mode"`
	PayloadOn         string         `json:"payload_on,omitempty"`
	PayloadOff        string         `json:"payload_off,omitempty"`
	DeviceClass       string         `json:"device_class,omitempty"`
	StateClass        string         `json:"state_class,omitempty"`
	UnitOfMeasurement string         `json:"unit_of_measurement,omitempty"`
	Icon              string         `json:"icon,omitempty"`
	QoS               uint8          `json:"qos"`
	Device            DeviceMetadata `json:"device"`
	Origin            OriginMetadata `json:"origin"`
}

type DeviceMetadata struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	SWVersion    string   `json:"sw_version"`
}

type OriginMetadata struct {
	Name       string `json:"name"`
	SWVersion  string `json:"sw_version"`
	SupportURL string `json:"support_url"`
}

type OutboundMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

type Status struct {
	Enabled       bool       `json:"enabled"`
	State         string     `json:"state"`
	Connected     bool       `json:"connected"`
	KnownNodes    int        `json:"known_nodes"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Dropped       uint64     `json:"dropped_messages"`
}

type nodeRecord struct {
	snapshot NodeSnapshot
	lastSeen time.Time
	online   bool
	lastRSSI time.Time
}

type Publisher struct {
	config Config
	hub    *stream.Hub
	logger *slog.Logger
	client mqtt.Client

	mu       sync.Mutex
	nodes    map[string]*nodeRecord
	queueMu  sync.Mutex
	queue    chan OutboundMessage
	statusMu sync.RWMutex
	status   Status
	dropped  atomic.Uint64
	start    time.Time
}

func New(config Config, hub *stream.Hub, logger *slog.Logger) (*Publisher, error) {
	if hub == nil || !validClientID(config.ClientID) || config.Prefix != DefaultDiscoveryPrefix || config.RefreshInterval <= 0 || config.RSSIInterval <= 0 || config.Port == 0 || config.BrokerURL == "" {
		return nil, fmt.Errorf("%w: validated config and stream hub are required", ErrConfig)
	}
	if logger == nil {
		logger = slog.Default()
	}
	publisher := &Publisher{
		config: config, hub: hub, logger: logger, nodes: make(map[string]*nodeRecord),
		queue:  make(chan OutboundMessage, maxOutboundMessages),
		status: Status{Enabled: true, State: "starting"}, start: time.Now(),
	}
	options := mqtt.NewClientOptions().
		AddBroker(config.BrokerURL).
		SetClientID(config.ClientID).
		SetCleanSession(true).
		SetKeepAlive(30 * time.Second).
		SetConnectTimeout(connectionTimeout).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(time.Minute).
		SetMessageChannelDepth(128).
		SetOrderMatters(false)
	options.SetWill(publisher.globalAvailabilityTopic(), "offline", 1, true)
	if config.Username != "" {
		options.SetUsername(config.Username)
		options.SetPassword(config.Password)
	}
	if config.TLS != nil {
		options.SetTLSConfig(config.TLS.Clone())
	}
	options.SetOnConnectHandler(publisher.onConnect)
	options.SetConnectionLostHandler(publisher.onConnectionLost)
	publisher.client = mqtt.NewClient(options)
	return publisher, nil
}

func (p *Publisher) Run(ctx context.Context) {
	if p == nil || p.client == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	subscription := p.hub.Subscribe()
	defer subscription.Close()
	go p.publishLoop(ctx)

	connect := p.client.Connect()
	connected := connect.WaitTimeout(connectionTimeout)
	if connected && connect.Error() != nil {
		p.logger.Warn("MQTT initial connection failed; reconnect will continue", "reason", "connect_failed")
		p.setStatus("reconnecting", "connection_failed")
	} else if !connected {
		p.setStatus("reconnecting", "")
	}

	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	refresh := time.NewTicker(p.config.RefreshInterval)
	defer refresh.Stop()
	lastAvailability := time.Now()
	for {
		select {
		case <-ctx.Done():
			p.shutdown()
			return
		case frame, ok := <-subscription.C:
			if !ok {
				p.shutdown()
				return
			}
			if len(frame) == 0 || len(frame) > maxStateFrameBytes {
				continue
			}
			nodes, err := DecodeSnapshots(frame, time.Now())
			if err != nil {
				continue
			}
			for _, node := range nodes {
				p.acceptSnapshot(node)
			}
		case now := <-heartbeat.C:
			p.markStaleNodes(now)
			if now.Sub(lastAvailability) >= availabilityHeartbeat {
				p.enqueue(OutboundMessage{Topic: p.globalAvailabilityTopic(), Payload: []byte("online"), QoS: 1, Retain: true})
				lastAvailability = now
			}
		case <-refresh.C:
			p.publishDiscoveryForKnownNodes()
		}
	}
}

func (p *Publisher) Status() Status {
	if p == nil {
		return Status{State: "disabled"}
	}
	p.statusMu.RLock()
	status := p.status
	p.statusMu.RUnlock()
	p.mu.Lock()
	status.KnownNodes = len(p.nodes)
	p.mu.Unlock()
	status.Connected = p.client != nil && p.client.IsConnected()
	status.Dropped = p.dropped.Load()
	if status.Connected {
		status.State = "connected"
	} else if status.State == "connected" {
		status.State = "reconnecting"
	}
	return status
}

func (p *Publisher) onConnect(client mqtt.Client) {
	p.setStatus("connected", "")
	p.enqueue(OutboundMessage{Topic: p.globalAvailabilityTopic(), Payload: []byte("online"), QoS: 1, Retain: true})
	p.mu.Lock()
	ids := make([]string, 0, len(p.nodes))
	for id := range p.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := p.nodes[id]
		p.enqueueDiscoveryLocked(id)
		fresh := time.Since(record.lastSeen) < DefaultNodeStaleAfter
		record.online = fresh
		availability := "offline"
		if fresh {
			availability = "online"
		}
		p.enqueue(OutboundMessage{Topic: p.nodeAvailabilityTopic(id), Payload: []byte(availability), QoS: 1, Retain: true})
		p.publishStates(record.snapshot)
	}
	p.mu.Unlock()
	_ = client
}

func (p *Publisher) onConnectionLost(_ mqtt.Client, _ error) {
	p.setStatus("reconnecting", "connection_lost")
}

func (p *Publisher) acceptSnapshot(snapshot NodeSnapshot) {
	if p == nil || !validNodeID(snapshot.ID) {
		return
	}
	p.mu.Lock()
	record, exists := p.nodes[snapshot.ID]
	if !exists {
		if len(p.nodes) >= maxKnownNodes {
			p.mu.Unlock()
			p.logger.Warn("MQTT node limit reached; additional node omitted")
			return
		}
		record = &nodeRecord{}
		p.nodes[snapshot.ID] = record
	}
	previous := record.snapshot
	first := !exists
	record.snapshot = snapshot
	record.lastSeen = time.Now()
	wasOnline := record.online
	record.online = true
	shouldSendRSSI := snapshot.RSSIDBm != nil && (first || record.lastRSSI.IsZero() || time.Since(record.lastRSSI) >= p.config.RSSIInterval)
	if shouldSendRSSI {
		record.lastRSSI = time.Now()
	}
	p.mu.Unlock()

	if first {
		p.enqueueDiscovery(snapshot.ID, "presence")
		p.enqueueDiscovery(snapshot.ID, "motion")
		if snapshot.RSSIDBm != nil {
			p.enqueueDiscovery(snapshot.ID, "rssi")
		}
	}
	if !wasOnline {
		p.enqueue(OutboundMessage{Topic: p.nodeAvailabilityTopic(snapshot.ID), Payload: []byte("online"), QoS: 1, Retain: true})
	}
	if first || previous.Presence != snapshot.Presence {
		stateValue := "OFF"
		if snapshot.Presence {
			stateValue = "ON"
		}
		p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "presence"), Payload: []byte(stateValue), QoS: 1, Retain: true})
	}
	if first || previous.Motion != snapshot.Motion {
		stateValue := "OFF"
		if snapshot.Motion {
			stateValue = "ON"
		}
		p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "motion"), Payload: []byte(stateValue), QoS: 1, Retain: true})
	}
	if shouldSendRSSI {
		p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "rssi"), Payload: []byte(strconv.FormatFloat(*snapshot.RSSIDBm, 'f', 1, 64)), QoS: 0})
	}
	p.statusMu.Lock()
	last := snapshot.At.UTC()
	p.status.LastMessageAt = &last
	p.statusMu.Unlock()
}

func (p *Publisher) markStaleNodes(now time.Time) {
	p.mu.Lock()
	var stale []string
	for id, record := range p.nodes {
		if record.online && now.Sub(record.lastSeen) > DefaultNodeStaleAfter {
			record.online = false
			stale = append(stale, id)
		}
	}
	p.mu.Unlock()
	sort.Strings(stale)
	for _, id := range stale {
		p.enqueue(OutboundMessage{Topic: p.nodeAvailabilityTopic(id), Payload: []byte("offline"), QoS: 1, Retain: true})
	}
}

func (p *Publisher) publishDiscoveryForKnownNodes() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.nodes))
	for id := range p.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.enqueueDiscoveryLocked(id)
	}
	p.mu.Unlock()
}

func (p *Publisher) enqueueDiscovery(nodeID, entity string) {
	config, topic, err := p.discovery(nodeID, entity)
	if err != nil {
		return
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return
	}
	p.enqueue(OutboundMessage{Topic: topic, Payload: encoded, QoS: 1, Retain: true})
}

func (p *Publisher) enqueueDiscoveryLocked(nodeID string) {
	p.enqueueDiscovery(nodeID, "presence")
	p.enqueueDiscovery(nodeID, "motion")
	if record := p.nodes[nodeID]; record != nil && record.snapshot.RSSIDBm != nil {
		p.enqueueDiscovery(nodeID, "rssi")
	}
}

func (p *Publisher) publishStates(snapshot NodeSnapshot) {
	if !validNodeID(snapshot.ID) {
		return
	}
	presence := "OFF"
	if snapshot.Presence {
		presence = "ON"
	}
	motion := "OFF"
	if snapshot.Motion {
		motion = "ON"
	}
	p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "presence"), Payload: []byte(presence), QoS: 1, Retain: true})
	p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "motion"), Payload: []byte(motion), QoS: 1, Retain: true})
	if snapshot.RSSIDBm != nil {
		p.enqueue(OutboundMessage{Topic: p.stateTopic(snapshot.ID, "rssi"), Payload: []byte(strconv.FormatFloat(*snapshot.RSSIDBm, 'f', 1, 64)), QoS: 0})
	}
}

func (p *Publisher) discovery(nodeID, entity string) (DiscoveryConfig, string, error) {
	if !validNodeID(nodeID) {
		return DiscoveryConfig{}, "", ErrConfig
	}
	component := "binary_sensor"
	name := "Presence estimate"
	deviceClass := "occupancy"
	payloadOn, payloadOff := "ON", "OFF"
	stateClass, unit, icon := "", "", "mdi:motion-sensor"
	switch entity {
	case "presence":
	case "motion":
		name, deviceClass, icon = "Motion", "motion", "mdi:run"
	case "rssi":
		component, name, deviceClass, icon = "sensor", "RSSI", "", "mdi:wifi"
		payloadOn, payloadOff = "", ""
		stateClass, unit = "measurement", "dBm"
	default:
		return DiscoveryConfig{}, "", ErrConfig
	}
	objectID := "ruview_" + p.config.ClientID + "_node_" + nodeID
	deviceID := "ruview_" + p.config.ClientID + "_" + nodeID
	config := DiscoveryConfig{
		Name: name, UniqueID: deviceID + "_" + entity, ObjectID: objectID,
		StateTopic: p.stateTopic(nodeID, entity),
		Availability: []Availability{
			{Topic: p.globalAvailabilityTopic(), PayloadAvailable: "online", PayloadNotAvailable: "offline"},
			{Topic: p.nodeAvailabilityTopic(nodeID), PayloadAvailable: "online", PayloadNotAvailable: "offline"},
		},
		AvailabilityMode: "all", PayloadOn: payloadOn, PayloadOff: payloadOff,
		DeviceClass: deviceClass, StateClass: stateClass, UnitOfMeasurement: unit,
		Icon: icon, QoS: 1,
		Device: DeviceMetadata{Identifiers: []string{deviceID}, Name: "Wave Ray node " + nodeID, Manufacturer: "ruvnet", Model: "Go sensing backend", SWVersion: "0.1.0-go"},
		Origin: OriginMetadata{Name: "Wave Ray", SWVersion: "0.1.0-go", SupportURL: "https://github.com/Garun-mp4/wave-ray"},
	}
	return config, p.config.Prefix + "/" + component + "/" + objectID + "/" + entity + "/config", nil
}

func (p *Publisher) globalAvailabilityTopic() string {
	return "ruview/" + p.config.ClientID + "/availability"
}

func (p *Publisher) nodeAvailabilityTopic(nodeID string) string {
	return "ruview/" + p.config.ClientID + "/node/" + nodeID + "/availability"
}

func (p *Publisher) stateTopic(nodeID, entity string) string {
	return "ruview/" + p.config.ClientID + "/node/" + nodeID + "/" + entity + "/state"
}

func (p *Publisher) enqueue(message OutboundMessage) {
	if p == nil || len(message.Topic) == 0 || len(message.Topic) > 512 || len(message.Payload) > 64<<10 {
		return
	}
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	select {
	case p.queue <- message:
		return
	default:
	}
	select {
	case <-p.queue:
	default:
	}
	select {
	case p.queue <- message:
	default:
		p.dropped.Add(1)
	}
}

func (p *Publisher) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-p.queue:
			if !p.client.IsConnected() {
				p.dropped.Add(1)
				continue
			}
			token := p.client.Publish(message.Topic, message.QoS, message.Retain, message.Payload)
			if !token.WaitTimeout(publishAckTimeout) {
				p.setStatus("connected", "publish_timeout")
				continue
			}
			if token.Error() != nil {
				p.setStatus("reconnecting", "publish_failed")
			}
		}
	}
}

func (p *Publisher) shutdown() {
	if p.client == nil {
		return
	}
	if p.client.IsConnected() {
		token := p.client.Publish(p.globalAvailabilityTopic(), 1, true, []byte("offline"))
		_ = token.WaitTimeout(publishAckTimeout)
		p.client.Disconnect(250)
	}
	p.setStatus("stopped", "")
}

func (p *Publisher) setStatus(stateValue, errorCode string) {
	p.statusMu.Lock()
	p.status.Enabled = true
	p.status.State = stateValue
	p.status.LastError = errorCode
	p.statusMu.Unlock()
}

func DecodeSnapshots(frame []byte, now time.Time) ([]NodeSnapshot, error) {
	var update state.SensingUpdate
	if len(frame) == 0 || len(frame) > maxStateFrameBytes || json.Unmarshal(frame, &update) != nil || update.Type != "sensing_update" || !finite(update.Timestamp) || update.Timestamp <= 0 || update.Timestamp > float64(math.MaxInt64)/1e9 {
		return nil, ErrUnavailable
	}
	if strings.Contains(strings.ToLower(update.Source), "simulat") || len(update.Nodes) > maxKnownNodes {
		return nil, ErrUnavailable
	}
	at := time.Unix(0, int64(math.Round(update.Timestamp*1e9))).UTC()
	classifications := make(map[uint8]state.NodeFeatureInfo, len(update.NodeFeatures))
	for _, feature := range update.NodeFeatures {
		classifications[feature.NodeID] = feature
	}
	nodes := make([]NodeSnapshot, 0, max(1, len(update.Nodes)))
	if len(update.Nodes) == 0 {
		var rssi *float64
		if validRSSI(update.Features.MeanRSSI) {
			value := update.Features.MeanRSSI
			rssi = &value
		}
		nodes = append(nodes, NodeSnapshot{ID: "room", Presence: update.Classification.Presence, Motion: moving(update.Classification.MotionLevel), RSSIDBm: rssi, At: at})
		return nodes, nil
	}
	for _, node := range update.Nodes {
		classification := update.Classification
		if feature, ok := classifications[node.NodeID]; ok {
			classification = feature.Classification
		}
		var rssi *float64
		if validRSSI(node.RSSIDBm) {
			value := node.RSSIDBm
			rssi = &value
		}
		nodes = append(nodes, NodeSnapshot{
			ID: strconv.Itoa(int(node.NodeID)), Presence: classification.Presence,
			Motion: moving(classification.MotionLevel), RSSIDBm: rssi, At: at,
		})
	}
	return nodes, nil
}

func moving(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "active", "present_moving", "walking", "running", "moving":
		return true
	default:
		return false
	}
}

func validRSSI(value float64) bool { return finite(value) && value >= -150 && value < 0 }

func validNodeID(value string) bool {
	if value == "room" {
		return true
	}
	if value == "" || len(value) > 3 {
		return false
	}
	parsed, err := strconv.ParseUint(value, 10, 8)
	return err == nil && strconv.FormatUint(parsed, 10) == value
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (p *Publisher) String() string {
	return fmt.Sprintf("MQTT publisher (%s:%d)", p.config.Host, p.config.Port)
}
