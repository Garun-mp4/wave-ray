package discovery

import (
	"strings"
	"testing"
)

func TestBuildServiceAdvertisesOnlySafeInstallationMetadata(t *testing.T) {
	service, err := BuildService("Wave Ray Installation", "home-lab_1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if service.ServiceName() != "_ruview._tcp.local" || service.FullName() != "Wave Ray Installation._ruview._tcp.local" || service.HostName() != "ruview-home-lab-1.local" || service.Port != 8080 {
		t.Fatalf("unexpected service record: %+v", service)
	}
	if len(service.TXT) != 3 || service.TXT[0] != "schema="+Schema || service.TXT[1] != "tls=0" || service.TXT[2] != "installation=home-lab_1" {
		t.Fatalf("unexpected DNS-SD TXT data: %v", service.TXT)
	}
	for _, field := range service.TXT {
		for _, forbidden := range []string{"ssid", "sensor", "csi", "pose", "token", "secret", "room"} {
			if strings.Contains(strings.ToLower(field), forbidden) {
				t.Fatalf("sensitive or sensing metadata leaked into TXT: %q", field)
			}
		}
	}
}

func TestBuildServiceRejectsInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name, installationID string
		port                 uint16
	}{
		{"", "home", 8080},
		{"Wave Ray", "home\nsecret", 8080},
		{"Wave Ray", strings.Repeat("a", 129), 8080},
		{"Wave Ray", "home", 0},
		{strings.Repeat("a", 64), "home", 8080},
	} {
		if _, err := BuildService(test.name, test.installationID, test.port); err == nil {
			t.Errorf("BuildService(%q, %q, %d) unexpectedly succeeded", test.name, test.installationID, test.port)
		}
	}
}

func TestShouldAdvertiseSkipsLoopbackAndExplicitDisable(t *testing.T) {
	base := Settings{BindAddress: "0.0.0.0", HTTPPort: 8080, InstallationID: "home", InstallationName: "Wave Ray"}
	if !ShouldAdvertise(base) {
		t.Fatal("wildcard listener should be discoverable")
	}
	base.BindAddress = "127.0.0.1"
	if ShouldAdvertise(base) {
		t.Fatal("loopback listener should not be discoverable")
	}
	base.BindAddress = "::1"
	if ShouldAdvertise(base) {
		t.Fatal("IPv6 loopback listener should not be discoverable")
	}
	base.BindAddress = "192.0.2.10"
	base.Disabled = true
	if ShouldAdvertise(base) {
		t.Fatal("explicitly disabled discovery should stay disabled")
	}
}
