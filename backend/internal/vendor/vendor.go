// Package vendor defines the bounded, capability-safe scalar RF provider
// contracts. These adapters do not decode complex CSI.
package vendor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxMetrics   = 64
	MaxKeyBytes  = 64
	MaxTextBytes = 256
	MaxPayload   = 1 << 20
	MaxPage      = 1000
	MaxBatch     = 256
)

var (
	ErrInvalidDescriptor   = errors.New("invalid provider descriptor")
	ErrCapabilityMismatch  = errors.New("event capability does not match provider")
	ErrInvalidCapability   = errors.New("complex CSI and unsupported states cannot be represented as scalar vendor events")
	ErrInvalidPayload      = errors.New("invalid or unbounded vendor payload")
	ErrMalformedPayload    = errors.New("malformed vendor payload")
	ErrCredentialsRequired = errors.New("provider credentials are required")
	ErrContractRequired    = errors.New("commercial contract or SDK access is required")
	ErrUnsupported         = errors.New("provider has no supported sensing interface")
)

type ID string

const (
	OriginAI    ID = "origin_ai"
	Plume       ID = "plume"
	Mist        ID = "mist"
	Netgear     ID = "netgear"
	ElectricImp ID = "electric_imp"
	RFSolutions ID = "rf_solutions"
	Linksys     ID = "linksys"
	Luma        ID = "luma"
	GoogleNest  ID = "google_nest"
	WiFiGarden  ID = "wifigarden"
)

type Capability string

const (
	ComplexCSI     Capability = "complex_csi"
	DerivedSensing Capability = "derived_sensing"
	RFTelemetry    Capability = "rf_telemetry"
	NetworkOnly    Capability = "network_only"
	Unsupported    Capability = "unsupported"
)

type Availability string

const (
	Available           Availability = "available"
	CredentialsRequired Availability = "credentials_required"
	ContractRequired    Availability = "contract_required"
	Experimental        Availability = "experimental"
	NotSupported        Availability = "unsupported"
)

type Descriptor struct {
	Vendor            ID           `json:"vendor"`
	Capabilities      []Capability `json:"capabilities"`
	Availability      Availability `json:"availability"`
	HardwareValidated bool         `json:"hardware_validated"`
	Reason            string       `json:"reason"`
}

func (d Descriptor) Validate() error {
	if !knownID(d.Vendor) || len(d.Capabilities) == 0 || len(d.Reason) == 0 || len(d.Reason) > MaxTextBytes || !validAvailability(d.Availability) {
		return ErrInvalidDescriptor
	}
	if d.HardwareValidated && d.Availability != Available {
		return ErrInvalidDescriptor
	}
	seen := make(map[Capability]struct{}, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		if !validCapability(capability) {
			return ErrInvalidDescriptor
		}
		if _, ok := seen[capability]; ok {
			return ErrInvalidDescriptor
		}
		seen[capability] = struct{}{}
	}
	return nil
}

type Event struct {
	Vendor      ID                 `json:"vendor"`
	Capability  Capability         `json:"capability"`
	Sequence    uint64             `json:"sequence"`
	TimestampUS uint64             `json:"timestamp_us"`
	SourceID    string             `json:"source_id"`
	Synthetic   bool               `json:"synthetic"`
	Metrics     map[string]float64 `json:"metrics"`
	Label       string             `json:"label,omitempty"`
}

func (e Event) Validate(d Descriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if e.Vendor != d.Vendor || !containsCapability(d.Capabilities, e.Capability) {
		return ErrCapabilityMismatch
	}
	if e.Capability == ComplexCSI || e.Capability == Unsupported {
		return ErrInvalidCapability
	}
	if e.SourceID == "" || len(e.SourceID) > MaxTextBytes || len(e.Metrics) == 0 || len(e.Metrics) > MaxMetrics || len(e.Label) > MaxTextBytes {
		return ErrInvalidPayload
	}
	for key, value := range e.Metrics {
		if key == "" || len(key) > MaxKeyBytes || math.IsNaN(value) || math.IsInf(value, 0) {
			return ErrInvalidPayload
		}
	}
	return nil
}

type Snapshot struct {
	EventType string `json:"event_type"`
	Source    string `json:"source"`
	Event
}

func NewSnapshot(event Event) (Snapshot, error) {
	descriptor, ok := DescriptorFor(event.Vendor)
	if !ok {
		return Snapshot{}, ErrCapabilityMismatch
	}
	if err := event.Validate(descriptor); err != nil {
		return Snapshot{}, err
	}
	provenance := "live"
	if event.Synthetic {
		provenance = "simulated"
	}
	return Snapshot{EventType: "vendor_rf", Source: "vendor:" + string(event.Vendor) + ":" + provenance, Event: cloneEvent(event)}, nil
}

var descriptors = []Descriptor{
	{Vendor: OriginAI, Capabilities: []Capability{DerivedSensing}, Availability: ContractRequired, Reason: "Origin partner API/SDK access is contract-gated; adapter accepts derived sensing only"},
	{Vendor: Plume, Capabilities: []Capability{RFTelemetry}, Availability: CredentialsRequired, Reason: "OpenSync radio/client telemetry only; Plume Sense is a separate gated service"},
	{Vendor: Mist, Capabilities: []Capability{RFTelemetry, NetworkOnly}, Availability: CredentialsRequired, Reason: "Mist cloud client RF telemetry and location context; never CSI"},
	{Vendor: Netgear, Capabilities: []Capability{RFTelemetry, NetworkOnly}, Availability: CredentialsRequired, Reason: "NETGEAR Insight client RF and network telemetry; never CSI"},
	{Vendor: ElectricImp, Capabilities: []Capability{RFTelemetry}, Availability: CredentialsRequired, Reason: "Optional authenticated agent/impCentral scalar telemetry bridge; never CSI"},
	{Vendor: RFSolutions, Capabilities: []Capability{RFTelemetry}, Availability: Experimental, Reason: "Optional non-Wi-Fi environmental telemetry fusion; excluded as a CSI source"},
	{Vendor: Linksys, Capabilities: []Capability{Unsupported}, Availability: NotSupported, Reason: "Linksys Aware reached end of support in 2024; no supported sensing interface"},
	{Vendor: Luma, Capabilities: []Capability{RFTelemetry}, Availability: Experimental, Reason: "Generic OpenWrt scalar telemetry fixture for already-owned Luma hardware; no Luma CSI claim"},
	{Vendor: GoogleNest, Capabilities: []Capability{NetworkOnly}, Availability: Experimental, Reason: "Network-only replay events; Device Access exposes no router CSI or RF telemetry"},
	{Vendor: WiFiGarden, Capabilities: []Capability{Unsupported}, Availability: ContractRequired, Reason: "Commercial SDK, chipset, schema, calibration and data-rights disclosure required"},
}

func Descriptors() []Descriptor {
	result := make([]Descriptor, len(descriptors))
	for i, descriptor := range descriptors {
		result[i] = descriptor
		result[i].Capabilities = append([]Capability(nil), descriptor.Capabilities...)
	}
	return result
}

func DescriptorFor(id ID) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Vendor == id {
			copy := descriptor
			copy.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
			return copy, true
		}
	}
	return Descriptor{}, false
}

func knownID(id ID) bool { _, ok := DescriptorFor(id); return ok }

func Decode(id ID, payload []byte) ([]Event, error) {
	if len(payload) == 0 || len(payload) > MaxPayload {
		return nil, ErrInvalidPayload
	}
	if (id == OriginAI || id == Plume) && len(payload) > 256<<10 {
		return nil, ErrInvalidPayload
	}
	descriptor, ok := DescriptorFor(id)
	if !ok {
		return nil, ErrCapabilityMismatch
	}
	switch id {
	case OriginAI:
		return decodeOrigin(payload, descriptor)
	case Plume:
		return decodePlume(payload, descriptor)
	case Mist, Netgear:
		return decodeClientPage(payload, descriptor)
	case ElectricImp, RFSolutions, Luma, GoogleNest:
		return decodeScalar(payload, descriptor)
	case Linksys:
		return nil, ErrUnsupported
	case WiFiGarden:
		return nil, ErrContractRequired
	default:
		return nil, ErrCapabilityMismatch
	}
}

type originEnvelope struct {
	Events []originEvent `json:"events"`
}
type originEvent struct {
	Sequence    uint64   `json:"sequence"`
	TimestampUS uint64   `json:"timestamp_us"`
	SourceID    string   `json:"source_id"`
	Kind        string   `json:"kind"`
	Confidence  float64  `json:"confidence"`
	Value       *float64 `json:"value"`
	Label       string   `json:"label"`
}

func decodeOrigin(payload []byte, descriptor Descriptor) ([]Event, error) {
	var envelope originEnvelope
	if err := decodeStrict(payload, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Events) == 0 || len(envelope.Events) > MaxBatch {
		return nil, ErrInvalidPayload
	}
	result := make([]Event, 0, len(envelope.Events))
	for _, item := range envelope.Events {
		if item.TimestampUS == 0 || item.Confidence < 0 || item.Confidence > 1 || item.Value == nil {
			return nil, ErrInvalidPayload
		}
		metric := ""
		switch item.Kind {
		case "motion":
			metric = "motion"
		case "occupancy":
			metric = "occupancy"
		case "presence":
			metric = "presence"
		case "fall":
			metric = "fall"
		case "breathing_rate":
			metric = "breathing_rate_bpm"
		default:
			return nil, ErrInvalidPayload
		}
		value := *item.Value
		if !finite(value) {
			return nil, ErrInvalidPayload
		}
		if metric == "breathing_rate_bpm" {
			if value < 1 || value > 120 {
				return nil, ErrInvalidPayload
			}
		} else if value != 0 && value != 1 {
			return nil, ErrInvalidPayload
		}
		event := Event{Vendor: OriginAI, Capability: DerivedSensing, Sequence: item.Sequence, TimestampUS: item.TimestampUS, SourceID: item.SourceID, Metrics: map[string]float64{metric: value, "confidence": item.Confidence}, Label: item.Label}
		if err := event.Validate(descriptor); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

type plumeEnvelope struct {
	Observations []plumeObservation `json:"observations"`
}
type plumeObservation struct {
	Sequence    uint64   `json:"sequence"`
	TimestampUS uint64   `json:"timestamp_us"`
	SourceID    string   `json:"source_id"`
	RSSI        float64  `json:"rssi_dbm"`
	Channel     uint16   `json:"channel"`
	Noise       *float64 `json:"noise_floor_dbm"`
	TX          *float64 `json:"tx_rate_mbps"`
	RX          *float64 `json:"rx_rate_mbps"`
	Clients     *uint16  `json:"clients"`
}

func decodePlume(payload []byte, descriptor Descriptor) ([]Event, error) {
	var envelope plumeEnvelope
	if err := decodeStrict(payload, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Observations) == 0 || len(envelope.Observations) > MaxBatch {
		return nil, ErrInvalidPayload
	}
	result := make([]Event, 0, len(envelope.Observations))
	for _, item := range envelope.Observations {
		if item.TimestampUS == 0 || item.RSSI < -127 || item.RSSI > 0 || item.Channel == 0 || item.Channel > 233 {
			return nil, ErrInvalidPayload
		}
		metrics := map[string]float64{"rssi_dbm": item.RSSI, "channel": float64(item.Channel)}
		if !putOptional(metrics, "noise_floor_dbm", item.Noise, -127, 0) || !putOptional(metrics, "tx_rate_mbps", item.TX, 0, 100000) || !putOptional(metrics, "rx_rate_mbps", item.RX, 0, 100000) {
			return nil, ErrInvalidPayload
		}
		if item.Clients != nil {
			metrics["clients"] = float64(*item.Clients)
		}
		event := Event{Vendor: Plume, Capability: RFTelemetry, Sequence: item.Sequence, TimestampUS: item.TimestampUS, SourceID: item.SourceID, Metrics: metrics}
		if err := event.Validate(descriptor); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

type scalarEnvelope struct {
	Events []scalarEvent `json:"events"`
}
type scalarEvent struct {
	Sequence    uint64             `json:"sequence"`
	TimestampUS uint64             `json:"timestamp_us"`
	SourceID    string             `json:"source_id"`
	Synthetic   bool               `json:"synthetic"`
	Metrics     map[string]float64 `json:"metrics"`
	Label       string             `json:"label"`
}

func decodeScalar(payload []byte, descriptor Descriptor) ([]Event, error) {
	if len(payload) > 64<<10 {
		return nil, ErrInvalidPayload
	}
	var envelope scalarEnvelope
	if err := decodeStrict(payload, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Events) == 0 || len(envelope.Events) > MaxBatch {
		return nil, ErrInvalidPayload
	}
	allowed := metricRules(descriptor.Vendor)
	result := make([]Event, 0, len(envelope.Events))
	for _, item := range envelope.Events {
		if item.TimestampUS == 0 || hasControl(item.SourceID) || hasControl(item.Label) {
			return nil, ErrInvalidPayload
		}
		for key, value := range item.Metrics {
			rule, ok := allowed[key]
			if !ok {
				return nil, ErrCapabilityMismatch
			}
			if !finite(value) || value < rule.min || value > rule.max || (rule.integer && math.Trunc(value) != value) {
				return nil, ErrInvalidPayload
			}
		}
		event := Event{Vendor: descriptor.Vendor, Capability: descriptor.Capabilities[0], Sequence: item.Sequence, TimestampUS: item.TimestampUS, SourceID: item.SourceID, Synthetic: item.Synthetic, Metrics: item.Metrics, Label: item.Label}
		if err := event.Validate(descriptor); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

type metricRule struct {
	min, max float64
	integer  bool
}

func metricRules(id ID) map[string]metricRule {
	common := map[string]metricRule{}
	switch id {
	case ElectricImp:
		common = map[string]metricRule{"battery_v": {0, 100, false}, "humidity_percent": {0, 100, false}, "rssi_dbm": {-127, 0, false}, "temperature_c": {-100, 200, false}, "voltage_v": {0, 1000, false}}
	case RFSolutions:
		common = map[string]metricRule{"battery_v": {0, 100, false}, "humidity_percent": {0, 100, false}, "relay_state": {0, 1, true}, "rssi_dbm": {-127, 0, false}, "temperature_c": {-100, 200, false}}
	case Luma:
		common = map[string]metricRule{"client_count": {0, 1_000_000, true}, "noise_dbm": {-127, 0, false}, "rssi_dbm": {-127, 0, false}, "rx_bytes": {0, 9_007_199_254_740_991, true}, "tx_bytes": {0, 9_007_199_254_740_991, true}}
	case GoogleNest:
		common = map[string]metricRule{"client_count": {0, 1_000_000, true}, "probe_count": {0, 1_000_000_000, true}, "rx_bytes": {0, 9_007_199_254_740_991, true}, "tx_bytes": {0, 9_007_199_254_740_991, true}}
	}
	return common
}

type vendorPage struct {
	Records    []map[string]json.RawMessage
	NextCursor string
}

func decodeClientPage(payload []byte, descriptor Descriptor) ([]Event, error) {
	page, err := parseClientPage(payload)
	if err != nil {
		return nil, err
	}
	if len(page.Records) == 0 || len(page.Records) > MaxPage {
		return nil, ErrInvalidPayload
	}
	result := make([]Event, 0, len(page.Records))
	for index, record := range page.Records {
		event, err := parseClientRecord(descriptor.Vendor, record, index)
		if err != nil {
			return nil, err
		}
		if err = event.Validate(descriptor); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

func parseClientPage(payload []byte) (vendorPage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return vendorPage{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return vendorPage{}, ErrInvalidPayload
	}
	var records []any
	switch value := root.(type) {
	case []any:
		records = value
	case map[string]any:
		for _, key := range []string{"results", "data", "clients", "events", "items"} {
			if candidate, ok := value[key].([]any); ok {
				records = candidate
				break
			}
		}
		var nextCursor string
		cursorFound := false
		for _, key := range []string{"next_cursor", "nextPageToken", "next_page_token"} {
			if raw, exists := value[key]; exists {
				cursorFound = true
				if raw != nil {
					cursor, ok := raw.(string)
					if !ok || !validCursor(cursor) {
						return vendorPage{}, ErrInvalidPayload
					}
					nextCursor = cursor
				}
				break
			}
		}
		if !cursorFound {
			if pagination, ok := value["pagination"].(map[string]any); ok {
				for _, key := range []string{"next", "cursor"} {
					if raw, exists := pagination[key]; exists {
						if raw != nil {
							cursor, ok := raw.(string)
							if !ok || !validCursor(cursor) {
								return vendorPage{}, ErrInvalidPayload
							}
							nextCursor = cursor
						}
						break
					}
				}
			}
		}
		return toVendorPage(records, nextCursor)
	default:
		return vendorPage{}, ErrInvalidPayload
	}
	return toVendorPage(records, "")
}

func toVendorPage(values []any, nextCursor string) (vendorPage, error) {
	page := vendorPage{Records: make([]map[string]json.RawMessage, 0, len(values)), NextCursor: nextCursor}
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return vendorPage{}, ErrInvalidPayload
		}
		record := make(map[string]json.RawMessage, len(object))
		for key, value := range object {
			raw, err := json.Marshal(value)
			if err != nil {
				return vendorPage{}, ErrInvalidPayload
			}
			record[key] = raw
		}
		page.Records = append(page.Records, record)
	}
	return page, nil
}

func parseClientRecord(id ID, record map[string]json.RawMessage, index int) (Event, error) {
	var source string
	var timestamp any
	var metrics = make(map[string]float64)
	var labelParts []string
	var err error
	if id == Mist {
		source, err = stringAlias(record, "id", "client_id", "mac")
		timestamp, _ = rawAlias(record, "timestamp", "last_seen", "lastSeen")
		if err != nil {
			return Event{}, err
		}
		for _, metric := range []struct {
			name     string
			keys     []string
			min, max float64
		}{{"rssi_dbm", []string{"rssi", "rssi_dbm"}, -150, 20}, {"snr_db", []string{"snr"}, -50, 100}, {"channel", []string{"channel"}, 1, 7000}, {"x_m", []string{"x"}, -1e6, 1e6}, {"y_m", []string{"y"}, -1e6, 1e6}} {
			if err := readOptionalMetric(record, metrics, metric.name, metric.min, metric.max, metric.keys...); err != nil {
				return Event{}, err
			}
		}
		for _, aliases := range [][]string{{"site_id"}, {"ap_id"}, {"event_type", "type"}} {
			part, present, textErr := optionalTextAlias(record, aliases...)
			if textErr != nil {
				return Event{}, textErr
			}
			if present {
				labelParts = append(labelParts, part)
			}
		}
	} else {
		source, err = stringAlias(record, "id", "clientId", "mac", "macAddress", "deviceId")
		timestamp, _ = rawAlias(record, "timestamp", "lastSeen", "observedAt")
		if err != nil {
			return Event{}, err
		}
		for _, metric := range []struct {
			name     string
			keys     []string
			min, max float64
		}{{"rssi_dbm", []string{"rssi", "signalStrength", "rssi_dbm"}, -150, 20}, {"snr_db", []string{"snr", "signalToNoiseRatio"}, -50, 100}, {"channel", []string{"channel"}, 1, 7000}, {"tx_rate_mbps", []string{"tx_rate_mbps", "txRateMbps", "tx_rate"}, 0, 100000}, {"rx_rate_mbps", []string{"rx_rate_mbps", "rxRateMbps", "rx_rate"}, 0, 100000}} {
			if err := readOptionalMetric(record, metrics, metric.name, metric.min, metric.max, metric.keys...); err != nil {
				return Event{}, err
			}
		}
		for _, aliases := range [][]string{{"location_id", "locationId"}, {"ap_id", "accessPointId", "apId"}, {"ssid", "ssidName"}} {
			part, present, textErr := optionalTextAlias(record, aliases...)
			if textErr != nil {
				return Event{}, textErr
			}
			if present {
				labelParts = append(labelParts, part)
			}
		}
	}
	if err := validText(source, 256); err != nil {
		return Event{}, err
	}
	for _, value := range labelParts {
		if err := validText(value, 256); err != nil {
			return Event{}, err
		}
	}
	timestampUS, err := parseTimestamp(timestamp)
	if err != nil || len(metrics) == 0 {
		return Event{}, ErrInvalidPayload
	}
	label := strings.Join(labelParts, "/")
	if len(label) > MaxTextBytes {
		return Event{}, ErrInvalidPayload
	}
	sequence := stableSequence(source, timestampUS, index)
	return Event{Vendor: id, Capability: RFTelemetry, Sequence: sequence, TimestampUS: timestampUS, SourceID: source, Metrics: metrics, Label: label}, nil
}

func readOptionalMetric(record map[string]json.RawMessage, metrics map[string]float64, name string, minimum, maximum float64, aliases ...string) error {
	raw, ok := rawAlias(record, aliases...)
	if !ok {
		return nil
	}
	value, err := decodeNumber(raw)
	if err != nil || !finite(value) || value < minimum || value > maximum {
		return ErrInvalidPayload
	}
	metrics[name] = value
	return nil
}

func parseTimestamp(raw any) (uint64, error) {
	if raw == nil {
		return 0, ErrInvalidPayload
	}
	var number float64
	switch value := raw.(type) {
	case json.RawMessage:
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 {
			return 0, ErrInvalidPayload
		}
		if trimmed[0] == '"' {
			var text string
			if json.Unmarshal(trimmed, &text) != nil {
				return 0, ErrInvalidPayload
			}
			return parseTimestamp(text)
		}
		if integer, err := strconv.ParseUint(string(trimmed), 10, 64); err == nil {
			return normalizeIntegerTimestamp(integer)
		}
		parsed, err := strconv.ParseFloat(string(trimmed), 64)
		if err != nil {
			return 0, ErrInvalidPayload
		}
		number = parsed
	case []byte:
		return parseTimestamp(json.RawMessage(value))
	case json.Number:
		if integer, err := strconv.ParseUint(string(value), 10, 64); err == nil {
			return normalizeIntegerTimestamp(integer)
		}
		parsed, err := value.Float64()
		if err != nil {
			return 0, ErrInvalidPayload
		}
		number = parsed
	case string:
		if integer, err := strconv.ParseUint(value, 10, 64); err == nil {
			return normalizeIntegerTimestamp(integer)
		}
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			number = parsed
		} else {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return 0, ErrInvalidPayload
			}
			micros := parsed.UnixMicro()
			if micros <= 0 {
				return 0, ErrInvalidPayload
			}
			return uint64(micros), nil
		}
	default:
		return 0, ErrInvalidPayload
	}
	if !finite(number) || number <= 0 {
		return 0, ErrInvalidPayload
	}
	micros := number
	if number < 1e10 {
		micros *= 1e6
	} else if number < 1e13 {
		micros *= 1e3
	}
	if !finite(micros) || micros >= math.Exp2(64) {
		return 0, ErrInvalidPayload
	}
	return uint64(math.Round(micros)), nil
}

func normalizeIntegerTimestamp(raw uint64) (uint64, error) {
	if raw == 0 {
		return 0, ErrInvalidPayload
	}
	if raw < 10_000_000_000 {
		if raw > math.MaxUint64/1_000_000 {
			return 0, ErrInvalidPayload
		}
		return raw * 1_000_000, nil
	}
	if raw < 10_000_000_000_000 {
		if raw > math.MaxUint64/1_000 {
			return 0, ErrInvalidPayload
		}
		return raw * 1_000, nil
	}
	if raw < 10_000_000_000_000_000 {
		return raw, nil
	}
	return 0, ErrInvalidPayload
}

func stableSequence(source string, timestamp uint64, index int) uint64 {
	hash := fnv.New64a()
	_, _ = io.WriteString(hash, source)
	var data [16]byte
	for i := 0; i < 8; i++ {
		data[i] = byte(timestamp >> (8 * i))
	}
	value := uint64(index)
	for i := 0; i < 8; i++ {
		data[8+i] = byte(value >> (8 * i))
	}
	_, _ = hash.Write(data[:])
	return hash.Sum64()
}

func decodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return ErrInvalidPayload
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalidPayload
	}
	return nil
}
func putOptional(metrics map[string]float64, name string, value *float64, minimum, maximum float64) bool {
	if value == nil {
		return true
	}
	if !finite(*value) || *value < minimum || *value > maximum {
		return false
	}
	metrics[name] = *value
	return true
}
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func validAvailability(value Availability) bool {
	switch value {
	case Available, CredentialsRequired, ContractRequired, Experimental, NotSupported:
		return true
	}
	return false
}

func validCapability(value Capability) bool {
	switch value {
	case ComplexCSI, DerivedSensing, RFTelemetry, NetworkOnly, Unsupported:
		return true
	}
	return false
}
func containsCapability(values []Capability, target Capability) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func validCursor(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\r\n&?#")
}
func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
func validText(value string, max int) error {
	if value == "" || len(value) > max || hasControl(value) {
		return ErrInvalidPayload
	}
	return nil
}
func cloneEvent(event Event) Event {
	clone := event
	clone.Metrics = make(map[string]float64, len(event.Metrics))
	for key, value := range event.Metrics {
		clone.Metrics[key] = value
	}
	return clone
}
func rawAlias(values map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	return nil, false
}
func stringAlias(values map[string]json.RawMessage, keys ...string) (string, error) {
	raw, ok := rawAlias(values, keys...)
	if !ok {
		return "", ErrInvalidPayload
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", ErrInvalidPayload
	}
	return value, nil
}
func optionalTextAlias(values map[string]json.RawMessage, keys ...string) (string, bool, error) {
	raw, ok := rawAlias(values, keys...)
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false, ErrInvalidPayload
	}
	if err := validText(value, MaxTextBytes); err != nil {
		return "", false, err
	}
	return value, true, nil
}
func decodeNumber(raw json.RawMessage) (float64, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, ErrInvalidPayload
	}
	value, err := number.Float64()
	if err != nil {
		return 0, ErrInvalidPayload
	}
	return value, nil
}

type Store struct {
	mu     sync.RWMutex
	latest map[ID]Snapshot
}

func NewStore() *Store { return &Store{latest: make(map[ID]Snapshot)} }
func (s *Store) Put(event Event) error {
	if s == nil {
		return errors.New("vendor store is unavailable")
	}
	snapshot, err := NewSnapshot(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.latest[event.Vendor] = snapshot
	s.mu.Unlock()
	return nil
}
func (s *Store) Latest(id ID) (Snapshot, bool) {
	if s == nil {
		return Snapshot{}, false
	}
	s.mu.RLock()
	snapshot, ok := s.latest[id]
	s.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	snapshot.Event = cloneEvent(snapshot.Event)
	return snapshot, true
}
func (s *Store) All() map[string]Snapshot {
	result := make(map[string]Snapshot)
	if s == nil {
		return result
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, snapshot := range s.latest {
		snapshot.Event = cloneEvent(snapshot.Event)
		result[string(id)] = snapshot
	}
	return result
}
