package vendor

import (
	"errors"
	"fmt"
	"testing"
)

func TestDescriptorsAreStableAndCapabilitySafe(t *testing.T) {
	values := Descriptors()
	if len(values) != 10 {
		t.Fatalf("descriptor count=%d, want 10", len(values))
	}
	seen := make(map[ID]bool)
	for _, descriptor := range values {
		if seen[descriptor.Vendor] {
			t.Fatalf("duplicate descriptor for %q", descriptor.Vendor)
		}
		seen[descriptor.Vendor] = true
		if err := descriptor.Validate(); err != nil {
			t.Errorf("descriptor %q: %v", descriptor.Vendor, err)
		}
		for _, capability := range descriptor.Capabilities {
			if capability == ComplexCSI {
				t.Errorf("scalar vendor %q claims complex CSI", descriptor.Vendor)
			}
		}
	}
	if descriptor, ok := DescriptorFor(OriginAI); !ok || descriptor.Availability != ContractRequired {
		t.Fatalf("OriginAI descriptor=%+v, found=%v", descriptor, ok)
	}
}

func TestOriginAIDecodesDerivedSensingAndRejectsSchemaExtensions(t *testing.T) {
	payload := []byte(`{"events":[{"sequence":7,"timestamp_us":42,"source_id":"zone-a","kind":"occupancy","confidence":0.91,"value":1,"label":"occupied"}]}`)
	events, err := Decode(OriginAI, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Capability != DerivedSensing || events[0].Metrics["occupancy"] != 1 || events[0].Metrics["confidence"] != 0.91 || events[0].Synthetic {
		t.Fatalf("unexpected decoded event: %+v", events)
	}
	if _, err := Decode(OriginAI, []byte(`{"events":[{"sequence":1,"timestamp_us":2,"source_id":"x","kind":"motion","confidence":1,"value":1,"raw_csi":[[1,2]]}]}`)); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("unknown CSI field error=%v", err)
	}
	if _, err := Decode(OriginAI, []byte(`{"events":[{"sequence":1,"timestamp_us":2,"source_id":"x","kind":"motion","confidence":1.1,"value":1}]}`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("invalid confidence error=%v", err)
	}
}

func TestPlumeDecodesScalarTelemetryOnly(t *testing.T) {
	payload := []byte(`{"observations":[{"sequence":9,"timestamp_us":10,"source_id":"pod-a","rssi_dbm":-54,"channel":36,"noise_floor_dbm":-94,"tx_rate_mbps":1200,"clients":4}]}`)
	events, err := Decode(Plume, payload)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Capability != RFTelemetry || events[0].Metrics["rssi_dbm"] != -54 || events[0].Metrics["clients"] != 4 || events[0].Synthetic {
		t.Fatalf("unexpected plume event: %+v", events[0])
	}
	if _, err := Decode(Plume, []byte(`{"observations":[{"sequence":1,"timestamp_us":2,"source_id":"pod","rssi_dbm":4,"channel":36}]}`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("out-of-range RSSI error=%v", err)
	}
}

func TestScalarProviderAllowListsAndSyntheticProvenance(t *testing.T) {
	payload := []byte(`{"events":[{"sequence":8,"timestamp_us":1700000000000000,"source_id":"riot","synthetic":true,"metrics":{"battery_v":3.1,"humidity_percent":44},"label":"lab"}]}`)
	events, err := Decode(RFSolutions, payload)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSnapshot(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != "vendor:rf_solutions:simulated" || snapshot.EventType != "vendor_rf" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if _, err := Decode(ElectricImp, []byte(`{"events":[{"sequence":1,"timestamp_us":1,"source_id":"x","synthetic":true,"metrics":{"csi_real":1}}]}`)); !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("CSI-shaped metrics error=%v", err)
	}
	if _, err := Decode(GoogleNest, []byte(`{"events":[{"sequence":1,"timestamp_us":1,"source_id":"x","synthetic":true,"metrics":{"rssi_dbm":-40}}]}`)); !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("network-only metric error=%v", err)
	}
}

func TestMistAndNetgearNormalizeClientPages(t *testing.T) {
	mist := []byte(`{"results":[{"id":"aa:bb:cc:dd:ee:ff","timestamp":1710000000,"rssi":-47,"snr":31,"channel":44,"x":12.5,"y":8.25,"site_id":"site-1","ap_id":"ap-7"},{"client_id":"station-2","last_seen":1710000000123,"rssi_dbm":-62,"event_type":"client-info"}],"next_cursor":"page-2"}`)
	events, err := Decode(Mist, mist)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].TimestampUS != 1710000000000000 || events[1].TimestampUS != 1710000000123000 || events[0].Metrics["x_m"] != 12.5 || events[0].Label != "site-1/ap-7" {
		t.Fatalf("unexpected Mist events: %+v", events)
	}
	netgear := []byte(`{"data":[{"clientId":"client-1","observedAt":"1710000000000000","signalStrength":-53,"signalToNoiseRatio":24,"channel":149,"txRateMbps":866.7,"rxRateMbps":721.2,"locationId":"office","accessPointId":"ap-2","ssidName":"lab"}],"pagination":{"next":"cursor-2"}}`)
	events, err = Decode(Netgear, netgear)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TimestampUS != 1710000000000000 || events[0].Metrics["tx_rate_mbps"] != 866.7 || events[0].Label != "office/ap-2/lab" {
		t.Fatalf("unexpected NETGEAR event: %+v", events)
	}
}

func TestUnsupportedProvidersFailClosed(t *testing.T) {
	if _, err := Decode(Linksys, []byte(`{}`)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Linksys error=%v", err)
	}
	if _, err := Decode(WiFiGarden, []byte(`{}`)); !errors.Is(err, ErrContractRequired) {
		t.Fatalf("WiFiGarden error=%v", err)
	}
	if _, err := Decode(ID("unknown"), []byte(`{}`)); !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("unknown vendor error=%v", err)
	}
}

func TestDecoderBoundsAndTimestamps(t *testing.T) {
	if _, err := Decode(OriginAI, nil); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("empty payload error=%v", err)
	}
	if _, err := Decode(OriginAI, make([]byte, MaxPayload+1)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("oversized payload error=%v", err)
	}
	if _, err := Decode(Mist, []byte(`[ {"id":"a","timestamp":"2026-01-02T03:04:05Z","rssi":-40} ]`)); err != nil {
		t.Fatalf("RFC3339 timestamp decode: %v", err)
	}
	if _, err := Decode(Mist, []byte(`{"results":[{"id":"a","timestamp":1710000000,"rssi":-40}],"next_cursor":null}`)); err != nil {
		t.Fatalf("null optional cursor should be accepted: %v", err)
	}
	if _, err := Decode(Mist, []byte(`[{"id":"a","timestamp":1710000000,"rssi":-40,"site_id":""}]`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("empty optional context error=%v", err)
	}
	if _, err := Decode(OriginAI, make([]byte, 256<<10+1)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("OriginAI payload over provider-specific bound error=%v", err)
	}
	for _, raw := range []string{`[ {"id":"a","timestamp":0,"rssi":-40} ]`, `[ {"id":"a","timestamp":"not-a-time","rssi":-40} ]`, `[ {"id":"a","timestamp":1710000000,"rssi":-200} ]`} {
		if _, err := Decode(Mist, []byte(raw)); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("payload %s error=%v", raw, err)
		}
	}
}

func TestStoreReturnsDefensiveCopies(t *testing.T) {
	store := NewStore()
	event := Event{Vendor: Plume, Capability: RFTelemetry, Sequence: 1, TimestampUS: 10, SourceID: "pod", Metrics: map[string]float64{"rssi_dbm": -50}}
	if err := store.Put(event); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := store.Latest(Plume)
	if !ok {
		t.Fatal("latest event was not stored")
	}
	snapshot.Metrics["rssi_dbm"] = 0
	again, ok := store.Latest(Plume)
	if !ok || again.Metrics["rssi_dbm"] != -50 {
		t.Fatal("caller mutated the stored metrics map")
	}
	if got := store.All(); len(got) != 1 {
		t.Fatalf("All() returned %d vendors", len(got))
	}
}

func FuzzDecodeNeverPanics(f *testing.F) {
	for _, seed := range [][]byte{[]byte(`{}`), []byte(`{"events":[]}`), []byte(`[]`), []byte(`not-json`)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > MaxPayload {
			return
		}
		for _, id := range []ID{OriginAI, Plume, Mist, Netgear, ElectricImp, RFSolutions, Linksys, Luma, GoogleNest, WiFiGarden} {
			_, _ = Decode(id, payload)
		}
	})
}

func ExampleDescriptors() {
	for _, descriptor := range Descriptors() {
		fmt.Println(descriptor.Vendor, descriptor.Availability)
	}
	// Output:
	// origin_ai contract_required
	// plume credentials_required
	// mist credentials_required
	// netgear credentials_required
	// electric_imp credentials_required
	// rf_solutions experimental
	// linksys unsupported
	// luma experimental
	// google_nest experimental
	// wifigarden contract_required
}
