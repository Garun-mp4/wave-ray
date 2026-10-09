package rufield

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruvnet/ruview/backend/internal/signal"
	"github.com/ruvnet/ruview/backend/internal/state"
)

func TestSurfaceEmitsSignedAnonymousRuFieldEvent(t *testing.T) {
	surface, err := NewWithSeed(t.TempDir(), strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer surface.Close()
	update := testSensingUpdate()
	emitted, err := surface.Emit(update)
	if err != nil || !emitted {
		t.Fatalf("Emit() = %t, %v", emitted, err)
	}
	response := surface.Snapshot()
	if response.Spec != "rufield" || response.Endpoint != "/api/field" || response.DevSigningKey || len(response.Events) != 1 {
		t.Fatalf("unexpected surface response: %+v", response)
	}
	event := response.Events[0]
	if err := VerifyEvent(event); err != nil {
		t.Fatalf("signature verification failed: %v", err)
	}
	if event.SpecVersion != SpecVersion || event.Tensor.Modality != "wifi_csi" || len(event.Tensor.Axes) != 1 || event.Tensor.Axes[0] != "frequency" || event.Tensor.Shape[0] != 6 || len(event.Tensor.Values) != 6 || event.Tensor.PrivacyClass != "P2" || event.Observation.PrivacyClass != "P2" {
		t.Fatalf("unexpected event contract/privacy class: %+v", event)
	}
	if event.Tensor.Confidence != 0.82 || event.Observation.Confidence != event.Tensor.Confidence || event.Tensor.NoiseFloor < 0 {
		t.Fatalf("RuField tensor did not preserve bounded classifier confidence and noise floor: %+v", event.Tensor)
	}
	if event.Provenance.RawHash == "" || !strings.HasPrefix(event.Provenance.RawHash, "sha256:") || event.Provenance.Synthetic {
		t.Fatalf("unexpected provenance: %+v", event.Provenance)
	}
	if event.Observation.SpaceCell != nil || event.Observation.RangeM != nil || event.Observation.TrackID != nil || event.Tensor.CalibrationID != nil {
		t.Fatalf("unverified location or calibration leaked into RuField: %+v", event.Observation)
	}
	if _, present := event.Observation.Features["heart_rate_bpm"]; present {
		t.Fatal("numeric heart-rate data leaked into RuField P2 projection")
	}
	if _, present := event.Observation.Features["breathing_rate_bpm"]; present {
		t.Fatal("numeric breathing-rate data leaked into RuField P2 projection")
	}
	if _, present := event.Observation.Features["breathing_band"]; !present || len(event.Tensor.Values) != 6 || event.Tensor.Values[0] != -54.5 || event.Tensor.Values[2] != 2 {
		t.Fatalf("RuField v0.1 should preserve aggregate Wi-Fi features: tensor=%+v features=%v", event.Tensor, event.Observation.Features)
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "signature") == false || strings.Contains(string(encoded), "private_key") || strings.Contains(string(encoded), "heart_rate") || strings.Contains(string(encoded), "breathing_rate") {
		t.Fatalf("event does not have a public signature or exposes key material: %s", encoded)
	}
	event.EventID += "-tampered"
	if err := VerifyEvent(event); err == nil {
		t.Fatal("signature verification accepted a tampered event")
	}
}

func TestCanonicalEncodingMatchesRuFieldSerdeShape(t *testing.T) {
	surface, err := NewWithSeed(t.TempDir(), strings.Repeat("03", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer surface.Close()
	if emitted, err := surface.Emit(testSensingUpdate()); err != nil || !emitted {
		t.Fatalf("Emit()=%t, %v", emitted, err)
	}
	event := surface.Snapshot().Events[0]
	tensorBytes, err := marshalCompatible(canonicalTensorFrom(event.Tensor))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"spec_version":"rufield.mfs.v0.1","timestamp_ns":1700000000125000000,"modality":"wifi_csi","axes":["frequency"],"shape":[6],"values":[-54.5,0.4,2.0,0.3,0.25,3.0],"confidence":0.82,"noise_floor":0.4,"calibration_id":null,"privacy_class":"P2"}`
	if string(tensorBytes) != want {
		t.Fatalf("canonical tensor encoding differs from RuField serde contract:\n got %s\nwant %s", tensorBytes, want)
	}
	canonicalEvent, err := canonicalEventBytes(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonicalEvent), `"features":{"breathing_band":0.23076923,"motion_energy":0.6666667,"presence":1.0,"transient":0.5}`) {
		t.Fatalf("RuField feature map is not stably sorted/encoded: %s", canonicalEvent)
	}
}

func TestTimestampConversionPreservesWholeSecondsAndRejectsInvalidValues(t *testing.T) {
	if got, ok := timestampToNS(1_700_000_000.125); !ok || got != 1_700_000_000_125_000_000 {
		t.Fatalf("timestampToNS lost subsecond precision: got=%d ok=%t", got, ok)
	}
	for _, value := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), float64(math.MaxUint64)/1e9 + 10} {
		if got, ok := timestampToNS(value); ok {
			t.Errorf("timestampToNS(%v) = %d, accepted invalid timestamp", value, got)
		}
	}
}

func TestSurfaceDoesNotEmitNoPresenceAndMarksSyntheticSource(t *testing.T) {
	surface, err := NewWithSeed(t.TempDir(), strings.Repeat("02", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer surface.Close()
	update := testSensingUpdate()
	update.Classification.Presence = false
	if emitted, err := surface.Emit(update); err != nil || emitted {
		t.Fatalf("empty cycle emitted=%t error=%v", emitted, err)
	}
	if got := len(surface.Snapshot().Events); got != 0 {
		t.Fatalf("empty cycle produced %d event(s)", got)
	}

	update = testSensingUpdate()
	update.Source = "simulate"
	if emitted, err := surface.Emit(update); err != nil || !emitted {
		t.Fatalf("simulated cycle emitted=%t error=%v", emitted, err)
	}
	event := surface.Snapshot().Events[0]
	if !event.Provenance.Synthetic {
		t.Fatal("simulation source was incorrectly labeled as measured")
	}
}

func TestSigningSeedPersistsAndRejectsUnsafeKeyFiles(t *testing.T) {
	dataDir := t.TempDir()
	first, err := NewWithSeed(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	firstKey := first.SignerPubkeyHex()
	first.Close()
	info, err := os.Stat(filepath.Join(dataDir, SigningSeedFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("signing seed permissions=%#o, want owner-only", info.Mode().Perm())
	}
	second, err := NewWithSeed(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.SignerPubkeyHex() != firstKey {
		t.Fatal("persisted signer key changed across server restart")
	}

	if err := os.Chmod(filepath.Join(dataDir, SigningSeedFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithSeed(dataDir, ""); err == nil {
		t.Fatal("world-readable signing key file was accepted")
	}
}

func TestSeedParserMatchesConfiguredHexAndRequiresMinimumRawLength(t *testing.T) {
	seed, ok := parseSeed(strings.Repeat("ab", 32))
	if !ok || seed[0] != 0xab || seed[31] != 0xab {
		t.Fatalf("hex seed parse: ok=%t seed=%x", ok, seed)
	}
	if _, ok := parseSeed("short"); ok {
		t.Fatal("short raw signing seed was accepted")
	}
}

func testSensingUpdate() state.SensingUpdate {
	return state.SensingUpdate{
		Type: "sensing_update", Timestamp: 1_700_000_000.125, Source: "esp32",
		MeasurementAuthority: "unverified",
		Features:             signal.FeatureInfo{MeanRSSI: -54.5, Variance: 0.4, MotionBandPower: 2, BreathingBandPower: 0.3, DominantFreqHz: 0.25, ChangePoints: 1, SpectralPower: 3},
		Classification:       signal.Classification{MotionLevel: "present_moving", Presence: true, Confidence: 0.82},
		SignalField:          signal.SignalField{GridSize: [3]int{2, 2, 1}, Values: []float64{0.1, 0.3, 0.2, 0.9}},
		NodeFeatures:         []state.NodeFeatureInfo{{NodeID: 7, LastSeenMS: 0}},
	}
}
