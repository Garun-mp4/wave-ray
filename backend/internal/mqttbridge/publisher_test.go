package mqttbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func TestConfigFromEnvIsOptInAndPersistsClientID(t *testing.T) {
	values := map[string]string{"RUVIEW_MQTT": "true", "RUVIEW_MQTT_HOST": "broker.local"}
	getenv := func(name string) string { return values[name] }
	dataDir := t.TempDir()
	first, enabled, err := ConfigFromEnv(getenv, dataDir)
	if err != nil || !enabled {
		t.Fatalf("config should enable MQTT: enabled=%v err=%v", enabled, err)
	}
	second, enabled, err := ConfigFromEnv(getenv, dataDir)
	if err != nil || !enabled || second.ClientID != first.ClientID {
		t.Fatalf("persistent client ID changed: first=%q second=%q err=%v", first.ClientID, second.ClientID, err)
	}
	if first.BrokerURL != "tcp://broker.local:1883" || first.Prefix != DefaultDiscoveryPrefix {
		t.Fatalf("unexpected broker defaults: %+v", first)
	}
	if first.Password != "" {
		t.Fatal("unexpected credential value in default configuration")
	}
	info, err := os.Stat(filepath.Join(dataDir, clientIDFile))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("client ID should be stored with owner-only permissions: info=%v err=%v", info, err)
	}
}

func TestConfigFromEnvRejectsInvalidSettingsWithoutExposingSecrets(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"invalid bool":         {"RUVIEW_MQTT": "sometimes"},
		"bad host":             {"RUVIEW_MQTT": "true", "RUVIEW_MQTT_HOST": "broker.local/secret"},
		"unpaired credentials": {"RUVIEW_MQTT": "true", "RUVIEW_MQTT_USERNAME": "user"},
		"unsafe client id":     {"RUVIEW_MQTT": "true", "RUVIEW_MQTT_CLIENT_ID": "client/#"},
		"bad prefix":           {"RUVIEW_MQTT": "true", "RUVIEW_MQTT_PREFIX": "other"},
	} {
		t.Run(name, func(t *testing.T) {
			getenv := func(key string) string { return values[key] }
			if _, _, err := ConfigFromEnv(getenv, t.TempDir()); err == nil {
				t.Fatal("expected invalid MQTT settings to fail")
			}
		})
	}

	values := map[string]string{
		"RUVIEW_MQTT": "true", "RUVIEW_MQTT_USERNAME": "user",
		"MQTT_PASSWORD": "sensitive-broker-password",
	}
	getenv := func(key string) string { return values[key] }
	config, enabled, err := ConfigFromEnv(getenv, t.TempDir())
	if err != nil || !enabled || config.Password != values["MQTT_PASSWORD"] {
		t.Fatalf("credential reference was not resolved: enabled=%v err=%v", enabled, err)
	}
	status, _ := json.Marshal(Status{Enabled: true, State: "connected", Connected: true})
	if strings.Contains(string(status), values["MQTT_PASSWORD"]) {
		t.Fatal("MQTT status leaked a broker password")
	}
}

func TestConfigFromEnvTLSAndRemotePlaintextWarningBoundary(t *testing.T) {
	values := map[string]string{"RUVIEW_MQTT": "true", "RUVIEW_MQTT_TLS": "true", "RUVIEW_MQTT_HOST": "broker.example"}
	getenv := func(key string) string { return values[key] }
	config, enabled, err := ConfigFromEnv(getenv, t.TempDir())
	if err != nil || !enabled || config.BrokerURL != "ssl://broker.example:8883" || config.TLS == nil || config.TLS.MinVersion != 0x0303 {
		t.Fatalf("TLS defaults are incorrect: enabled=%v config=%+v err=%v", enabled, config, err)
	}
	if IsLoopbackHost("localhost") == false || IsLoopbackHost("192.168.1.5") {
		t.Fatal("loopback broker classification is incorrect")
	}
}

func TestDecodeSnapshotsOnlyExportsCurrentCoarseProjection(t *testing.T) {
	frame := []byte(`{"type":"sensing_update","timestamp":1710000000,"source":"esp32:live","classification":{"presence":true,"motion_level":"present_moving"},"estimated_persons":3,"persons":[{"track_id":"private"}],"vital_signs":{"heart_rate_bpm":72,"breathing_rate_bpm":15},"nodes":[{"node_id":7,"rssi_dbm":-47}],"node_features":[{"node_id":7,"classification":{"presence":false,"motion_level":"present_still"}}]}`)
	nodes, err := DecodeSnapshots(frame, time.Now())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("decode failed: nodes=%+v err=%v", nodes, err)
	}
	if nodes[0].ID != "7" || nodes[0].Presence || nodes[0].Motion || nodes[0].RSSIDBm == nil || *nodes[0].RSSIDBm != -47 {
		t.Fatalf("per-node classified projection is incorrect: %+v", nodes[0])
	}
	encoded, err := json.Marshal(nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"heart_rate", "breathing_rate", "track_id", "estimated_persons", "persons", "position", "pose"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("MQTT projection included unsupported or personal field %q: %s", forbidden, encoded)
		}
	}
}

func TestDecodeSnapshotsRejectsSyntheticAndMalformedFrames(t *testing.T) {
	tooManyNodes := make([]map[string]any, maxKnownNodes+1)
	for index := range tooManyNodes {
		tooManyNodes[index] = map[string]any{"node_id": index}
	}
	tooManyFrame, err := json.Marshal(map[string]any{
		"type": "sensing_update", "timestamp": 1710000000, "nodes": tooManyNodes,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range [][]byte{
		[]byte(`{"type":"sensing_update","timestamp":1710000000,"source":"simulated","classification":{"presence":true}}`),
		[]byte(`{"type":"pose_data","timestamp":1710000000}`),
		[]byte(`{"type":"sensing_update","timestamp":-1}`),
		tooManyFrame,
		[]byte(`{"type":"sensing_update","timestamp":1710000000,"nodes":[{}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}]}`),
	} {
		if nodes, err := DecodeSnapshots(frame, time.Now()); err == nil || nodes != nil {
			t.Fatalf("unsupported frame was accepted: nodes=%+v err=%v", nodes, err)
		}
	}
}

func TestDiscoveryUsesBoundedTopicsAndPrivacyProjection(t *testing.T) {
	publisher := &Publisher{config: Config{ClientID: "ruview-test", Prefix: DefaultDiscoveryPrefix}}
	for _, entity := range []string{"presence", "motion", "rssi"} {
		config, topic, err := publisher.discovery("12", entity)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(topic, "/"+entity+"/config") || !strings.Contains(topic, "homeassistant/") || config.AvailabilityMode != "all" || len(config.Availability) != 2 {
			t.Fatalf("unexpected discovery contract for %s: topic=%s config=%+v", entity, topic, config)
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"heart_rate", "breathing_rate", "pose", "person_count", "track_id"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("%s discovery leaked unsupported entity %q: %s", entity, forbidden, encoded)
			}
		}
	}
	if _, _, err := publisher.discovery("../../private", "presence"); err == nil {
		t.Fatal("unsafe node id was accepted for MQTT topics")
	}
}

func TestPublisherMosquittoIntegration(t *testing.T) {
	broker := strings.TrimSpace(os.Getenv("RUVIEW_MQTT_TEST_BROKER"))
	if broker == "" {
		t.Skip("set RUVIEW_MQTT_TEST_BROKER to run the Compose Mosquitto integration test")
	}
	id := fmt.Sprintf("ruview-test-%d", time.Now().UnixNano())
	received := make(chan mqtt.Message, 128)
	observerOptions := mqtt.NewClientOptions().AddBroker("tcp://" + broker).SetClientID(id + "-observer")
	observerOptions.SetDefaultPublishHandler(func(_ mqtt.Client, message mqtt.Message) {
		select {
		case received <- message:
		default:
		}
	})
	observer := mqtt.NewClient(observerOptions)
	if token := observer.Connect(); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("connect MQTT observer: %v", token.Error())
	}
	defer observer.Disconnect(100)
	if token := observer.Subscribe("#", 1, nil); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("subscribe MQTT observer: %v", token.Error())
	}

	hub := ws.NewHub()
	config := Config{
		BrokerURL: "tcp://" + broker, Host: "mqtt-test-broker", Port: 1883,
		ClientID: id, Prefix: DefaultDiscoveryPrefix,
		RefreshInterval: time.Minute, RSSIInterval: time.Second,
	}
	publisher, err := New(config, hub, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); publisher.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("MQTT publisher did not stop")
		}
	}()

	deadline := time.Now().Add(8 * time.Second)
	for !publisher.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !publisher.Status().Connected {
		t.Fatal("MQTT publisher did not connect to Mosquitto")
	}
	frame := []byte(fmt.Sprintf(`{"type":"sensing_update","timestamp":%d,"source":"esp32:live","classification":{"presence":true,"motion_level":"present_moving"},"nodes":[{"node_id":5,"rssi_dbm":-52}]}`, time.Now().Unix()))
	hub.Publish(frame)
	wantedConfig := "homeassistant/binary_sensor/ruview_" + id + "_node_5/presence/config"
	wantedState := "ruview/" + id + "/node/5/presence/state"
	seenConfig, seenState := false, false
	deadline = time.Now().Add(8 * time.Second)
	for (!seenConfig || !seenState) && time.Now().Before(deadline) {
		select {
		case message := <-received:
			if message.Topic() == wantedConfig {
				var discovery DiscoveryConfig
				if err := json.Unmarshal(message.Payload(), &discovery); err != nil || discovery.UniqueID != "ruview_"+id+"_5_presence" {
					t.Fatalf("invalid Home Assistant discovery payload: %s err=%v", message.Payload(), err)
				}
				seenConfig = true
			}
			if message.Topic() == wantedState && string(message.Payload()) == "ON" {
				seenState = true
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !seenConfig || !seenState {
		t.Fatalf("MQTT did not publish discovery/state: config=%v state=%v", seenConfig, seenState)
	}
}
