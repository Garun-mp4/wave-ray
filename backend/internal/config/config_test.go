package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultsLoopback(t *testing.T) {
	cfg, err := Load(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BindAddr != "127.0.0.1" || cfg.HTTPPort != 8080 || cfg.UDPPort != 5005 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.EdgeRegistryURL != DefaultEdgeRegistryURL || cfg.EdgeRegistryTTL != DefaultEdgeRegistryTTLSecond {
		t.Fatalf("unexpected edge registry defaults: %+v", cfg)
	}
	if cfg.NoMDNS || cfg.InstallationID != "installation" || cfg.InstallationName != "RuView Installation" {
		t.Fatalf("unexpected discovery defaults: %+v", cfg)
	}
}

func TestLoadPreservesExistingRuntimeEnvironmentNames(t *testing.T) {
	cfg, err := Load(nil, func(key string) string {
		return map[string]string{
			"RUVIEW_BIND_ADDR": "127.0.0.1",
			"RUVIEW_DATA_DIR":  "./state",
			"MODELS_DIR":       "./models",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BindAddr != "127.0.0.1" || cfg.DataDir != "./state" || cfg.ModelsDir != "./models" {
		t.Fatalf("existing runtime env settings were not preserved: %+v", cfg)
	}
}

func TestBooleanEnvironmentValuesUseCommonSpellings(t *testing.T) {
	env := map[string]string{
		"RUVIEW_PRIVACY_MODE":          "yes",
		"RUVIEW_ALLOW_UNAUTHENTICATED": "off",
		"RUVIEW_UDP_INSECURE_LAN":      "0",
		"RUVIEW_NO_EDGE_REGISTRY":      "on",
		"RUVIEW_NO_MDNS":               "yes",
	}
	cfg, err := Load(nil, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PrivacyMode || cfg.AllowUnauthenticated || cfg.UDPInsecureLAN || !cfg.NoEdgeRegistry || !cfg.NoMDNS {
		t.Fatalf("environment booleans were not parsed as expected: %+v", cfg)
	}
}

func TestInvalidBooleanEnvironmentValuesAreRejected(t *testing.T) {
	for _, name := range []string{
		"RUVIEW_PRIVACY_MODE", "RUVIEW_ALLOW_UNAUTHENTICATED", "RUVIEW_UDP_INSECURE_LAN", "RUVIEW_NO_EDGE_REGISTRY", "RUVIEW_NO_MDNS",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, func(key string) string {
				if key == name {
					return "tru"
				}
				return ""
			})
			if err == nil {
				t.Fatal("invalid boolean silently changed server configuration")
			}
		})
	}
}

func TestInstallationDiscoveryConfigurationIsBounded(t *testing.T) {
	cfg, err := Load(nil, func(key string) string {
		return map[string]string{
			"RUVIEW_NO_MDNS":           "false",
			"RUVIEW_INSTALLATION_ID":   "home-lab_1",
			"RUVIEW_INSTALLATION_NAME": "RuView upstairs",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NoMDNS || cfg.InstallationID != "home-lab_1" || cfg.InstallationName != "RuView upstairs" {
		t.Fatalf("discovery settings were not applied: %+v", cfg)
	}
	for name, value := range map[string]string{
		"RUVIEW_INSTALLATION_ID":   strings.Repeat("x", 129),
		"RUVIEW_INSTALLATION_NAME": strings.Repeat("x", 64),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			if err == nil {
				t.Fatalf("unbounded %s was accepted", name)
			}
		})
	}
}

func TestEdgeRegistryFlagsAndEnvironment(t *testing.T) {
	cfg, err := Load([]string{"--edge-registry-url", "https://mirror.example/registry.json", "--edge-registry-ttl-secs", "60"}, func(key string) string {
		if key == "RUVIEW_NO_EDGE_REGISTRY" {
			return "false"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EdgeRegistryURL != "https://mirror.example/registry.json" || cfg.EdgeRegistryTTL != 60 || cfg.NoEdgeRegistry {
		t.Fatalf("edge registry configuration was not applied: %+v", cfg)
	}

	cfg, err = Load(nil, func(key string) string {
		return map[string]string{
			"RUVIEW_EDGE_REGISTRY_URL":      "https://mirror.example/catalog.json",
			"RUVIEW_EDGE_REGISTRY_TTL_SECS": "120",
			"RUVIEW_NO_EDGE_REGISTRY":       "yes",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EdgeRegistryURL != "https://mirror.example/catalog.json" || cfg.EdgeRegistryTTL != 120 || !cfg.NoEdgeRegistry {
		t.Fatalf("edge registry environment was not applied: %+v", cfg)
	}
}

func TestActivityStatePathFlagAndEnvironment(t *testing.T) {
	cfg, err := Load([]string{"--activity-state", "./data/activity.json"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActivityStatePath != "./data/activity.json" {
		t.Fatalf("activity state flag not applied: %+v", cfg)
	}
	cfg, err = Load(nil, func(key string) string {
		if key == "RUVIEW_ACTIVITY_STATE" {
			return "./runtime/floors.json"
		}
		return ""
	})
	if err != nil || cfg.ActivityStatePath != "./runtime/floors.json" {
		t.Fatalf("activity state environment not applied: config=%+v err=%v", cfg, err)
	}
}

func TestPoseModelSettingsCanSelectSingleCSINode(t *testing.T) {
	cfg, err := Load(nil, func(key string) string {
		return map[string]string{
			"RUVIEW_POSE_MODEL_PATH": "./models/pose.safetensors",
			"RUVIEW_POSE_NODE_ID":    "17",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoseModelPath != "./models/pose.safetensors" || cfg.PoseNodeID != 17 {
		t.Fatalf("pose model settings were not loaded: %+v", cfg)
	}
	cfg, err = Load([]string{"--pose-node-id", "-1", "--pose-model", "./pose.safetensors"}, func(string) string { return "" })
	if err != nil || cfg.PoseNodeID != -1 || cfg.PoseModelPath != "./pose.safetensors" {
		t.Fatalf("pose model flags were not parsed: config=%+v err=%v", cfg, err)
	}
	cfg, err = Load(nil, func(key string) string {
		if key == "RUVIEW_POSE_NODE_ID" {
			return "-1"
		}
		return ""
	})
	if err != nil || cfg.PoseNodeID != -1 {
		t.Fatalf("automatic node selection environment was not parsed: config=%+v err=%v", cfg, err)
	}
}

func TestPoseModelSettingsRejectInvalidNodeID(t *testing.T) {
	for _, value := range []string{"256", "-2", "node"} {
		t.Run(value, func(t *testing.T) {
			_, err := Load(nil, func(key string) string {
				if key == "RUVIEW_POSE_NODE_ID" {
					return value
				}
				return ""
			})
			if err == nil {
				t.Fatal("invalid pose node ID was accepted")
			}
		})
	}
}

func TestEdgeRegistryRejectsInvalidURLAndTTL(t *testing.T) {
	for _, args := range [][]string{
		{"--edge-registry-url", "file:///etc/passwd"},
		{"--edge-registry-url", "https://user:secret@example.com/catalog"},
		{"--edge-registry-ttl-secs", "0"},
		{"--edge-registry-ttl-secs", "18446744073709551615"},
	} {
		if _, err := Load(args, func(string) string { return "" }); err == nil {
			t.Fatalf("invalid edge registry config was accepted: %v", args)
		}
	}
}

func TestRoutableHTTPRequiresAuthOrExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{{"refuses", map[string]string{"SENSING_BIND_ADDR": "0.0.0.0", "SENSING_UDP_BIND_ADDR": "127.0.0.1"}, true}, {"token", map[string]string{"SENSING_BIND_ADDR": "0.0.0.0", "SENSING_UDP_BIND_ADDR": "127.0.0.1", "RUVIEW_API_TOKEN": "test-token"}, false}, {"opt-in", map[string]string{"SENSING_BIND_ADDR": "0.0.0.0", "SENSING_UDP_BIND_ADDR": "127.0.0.1", "RUVIEW_ALLOW_UNAUTHENTICATED": "yes"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(nil, func(k string) string { return tc.env[k] })
			if (err != nil) != tc.want {
				t.Fatalf("err=%v; want error=%v", err, tc.want)
			}
		})
	}
}

func TestRoutableUDPRequiresAllowlistOrExplicitOptIn(t *testing.T) {
	_, err := Load([]string{"--udp-bind-addr", "0.0.0.0"}, func(string) string { return "" })
	if err == nil {
		t.Fatal("routable UDP bind without guard was accepted")
	}
	_, err = Load([]string{"--udp-bind-addr", "0.0.0.0", "--udp-allow", "192.0.2.1/32"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("allowlisted UDP bind refused: %v", err)
	}
}

func TestOAuthConfigurationIsAcceptedAndDefaultsClientAudience(t *testing.T) {
	cfg, err := Load(nil, func(k string) string {
		if k == "RUVIEW_OAUTH_ISSUER" {
			return "https://issuer.example"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("valid OAuth configuration rejected: %v", err)
	}
	if cfg.OAuthClientIDs == nil || len(cfg.OAuthClientIDs) != 1 || cfg.OAuthClientIDs[0] != "ruview" {
		t.Fatalf("default OAuth audience=%v, want [ruview]", cfg.OAuthClientIDs)
	}
}

func TestOAuthConfigurationRejectsUnsafeOrIncompleteURLs(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"credentials":         {"RUVIEW_OAUTH_ISSUER": "https://user:pass@issuer.example"},
		"query":               {"RUVIEW_OAUTH_ISSUER": "https://issuer.example?redirect=evil"},
		"insecure_remote":     {"RUVIEW_OAUTH_ISSUER": "http://issuer.example"},
		"jwks_without_issuer": {"RUVIEW_OAUTH_JWKS_URL": "https://issuer.example/keys"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(nil, func(k string) string { return env[k] })
			if err == nil {
				t.Fatal("invalid OAuth URL configuration accepted")
			}
		})
	}
}

func TestWiFiSourceIsAcceptedForRuntimePlatformValidation(t *testing.T) {
	cfg, err := Load([]string{"--source", "wifi"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("wifi source should pass portable config validation: %v", err)
	}
	if cfg.Source != "wifi" {
		t.Fatalf("source=%q, want wifi", cfg.Source)
	}
}

func TestSourceEnvironmentIsHonoredUnlessExplicitFlagOverridesIt(t *testing.T) {
	getenv := func(key string) string {
		if key == "CSI_SOURCE" {
			return "simulate"
		}
		return ""
	}

	cfg, err := Load(nil, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != "simulate" {
		t.Fatalf("source=%q, want environment value simulate", cfg.Source)
	}

	cfg, err = Load([]string{"--source", "esp32"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != "esp32" {
		t.Fatalf("source=%q, want explicit flag value esp32", cfg.Source)
	}
}
