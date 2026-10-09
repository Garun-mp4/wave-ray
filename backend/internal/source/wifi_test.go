package source

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ruvnet/ruview/backend/internal/state"
)

func TestParseNetshBSSIDOutput(t *testing.T) {
	output := `Interface name : Wi-Fi
SSID 1 : Synthetic Lab
    Network type : Infrastructure
    BSSID 1 : 02:11:22:33:44:55
         Signal : 80%
         Radio type : 802.11ax
         Channel : 36
    BSSID 2 : 02:11:22:33:44:66
         Signal : 60
         Channel : 11
SSID 2 : Hidden
    BSSID 1 : malformed
         Signal : 95%
`

	observations, err := ParseNetshBSSIDOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("got %d observations, want 2: %+v", len(observations), observations)
	}
	if got := observations[0]; got.BSSID != "02:11:22:33:44:55" || got.SSID != "Synthetic Lab" || got.SignalPct != 80 || got.RSSIDBm != -60 || got.Channel != 36 || got.RadioType != "802.11ax" {
		t.Fatalf("unexpected first observation: %+v", got)
	}
	if got := observations[1]; got.BSSID != "02:11:22:33:44:66" || got.SignalPct != 60 || got.RSSIDBm != -70 || got.Channel != 11 {
		t.Fatalf("unexpected second observation: %+v", got)
	}
	if observations[0].ObservedAt.IsZero() || !observations[0].ObservedAt.Equal(observations[1].ObservedAt) {
		t.Fatal("all observations in one scan must share a timestamp")
	}
}

func TestParseNetshBSSIDOutputSkipsMalformedAndBoundsResults(t *testing.T) {
	if _, err := ParseNetshBSSIDOutput(strings.Repeat("x", maxWiFiScanOutput+1)); !errors.Is(err, ErrWiFiOutputLimit) {
		t.Fatalf("oversized parser input error = %v, want %v", err, ErrWiFiOutputLimit)
	}
	var output strings.Builder
	for i := 0; i < maxWiFiBSSIDs+10; i++ {
		fmt.Fprintf(&output, "SSID %d : Network %d\n BSSID 1 : 02:00:00:00:%02x:%02x\n Signal : 50%%\n", i, i, i/256, i%256)
	}
	observations, err := ParseNetshBSSIDOutput(output.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != maxWiFiBSSIDs {
		t.Fatalf("got %d observations, want bounded maximum %d", len(observations), maxWiFiBSSIDs)
	}
	if empty, err := ParseNetshBSSIDOutput("localized output without recognized keys"); err != nil || len(empty) != 0 {
		t.Fatalf("unknown localized output should be ignored, got %v, %v", empty, err)
	}
}

func TestParseNetshInterfaceOutput(t *testing.T) {
	observation, err := ParseNetshInterfaceOutput("SSID : Synthetic Lab\nSignal : 80%\n")
	if err != nil {
		t.Fatal(err)
	}
	if observation == nil || observation.SSID != "Synthetic Lab" || observation.SignalPct != 80 || observation.RSSIDBm != -52 {
		t.Fatalf("unexpected connected-interface observation: %+v", observation)
	}
	if disconnected, err := ParseNetshInterfaceOutput("SSID : Synthetic Lab\n"); err != nil || disconnected != nil {
		t.Fatalf("missing signal should not produce an observation: %+v, %v", disconnected, err)
	}
	if _, err := ParseNetshInterfaceOutput("Signal : 101%\n"); err == nil {
		t.Fatal("out-of-range signal percentage was accepted")
	}
}

func TestHostWiFiScannerUsesConnectedInterfaceFallback(t *testing.T) {
	var calls [][]string
	scanner := &HostWiFiScanner{run: func(_ context.Context, command string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{command}, args...))
		if len(calls) == 1 {
			return []byte("No networks found"), nil
		}
		return []byte("SSID : Synthetic Lab\nSignal : 50%\n"), nil
	}}
	observations, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].SSID != "Synthetic Lab" {
		t.Fatalf("fallback observations = %+v", observations)
	}
	if len(calls) != 2 || strings.Join(calls[0], " ") != "wlan show networks mode=bssid" || strings.Join(calls[1], " ") != "wlan show interfaces" {
		t.Fatalf("unexpected fixed netsh arguments: %#v", calls)
	}
}

func TestHostWiFiScannerRejectsOversizedCommandOutput(t *testing.T) {
	scanner := &HostWiFiScanner{run: func(context.Context, string, ...string) ([]byte, error) {
		return bytesOf(maxWiFiScanOutput + 1), nil
	}}
	if _, err := scanner.Scan(context.Background()); !errors.Is(err, ErrWiFiOutputLimit) {
		t.Fatalf("scan error = %v, want %v", err, ErrWiFiOutputLimit)
	}
}

func TestWiFiObservationsProduceExplicitRSSIDerivedFrame(t *testing.T) {
	observations := []BSSIDObservation{
		{BSSID: "02:00:00:00:00:02", SignalPct: 60, RSSIDBm: -70, Channel: 36},
		{BSSID: "02:00:00:00:00:01", SignalPct: 80, RSSIDBm: -60, Channel: 11},
		{BSSID: "02:00:00:00:00:01", SignalPct: 90, RSSIDBm: -55, Channel: 1},
	}
	frame, ok := wifiObservationsToFrame(observations)
	if !ok {
		t.Fatal("valid observations did not produce a frame")
	}
	if frame.Source != "wifi" || frame.InputMode != "rssi_derived" || frame.NodeID != 0 || frame.Subcarriers != 2 {
		t.Fatalf("frame provenance/shape is incorrect: %+v", frame)
	}
	if frame.FrequencyMHz != 2462 || frame.RSSIDBm != -65 {
		t.Fatalf("frequency/RSSI = %d/%d, want 2462/-65", frame.FrequencyMHz, frame.RSSIDBm)
	}
	if frame.Amplitude[0] != 100 || math.Abs(frame.Amplitude[1]-31.6227766017) > 1e-8 {
		t.Fatalf("amplitudes = %v", frame.Amplitude)
	}
	if math.Abs(frame.Phase[0]-math.Pi*11/48) > 1e-12 || math.Abs(frame.Phase[1]-math.Pi*36/48) > 1e-12 {
		t.Fatalf("channel pseudo-phase = %v", frame.Phase)
	}
}

func TestWiFiSourceStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scanner := &HostWiFiScanner{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("No networks found"), nil
	}}
	store := state.New("wifi", nil)
	source, err := StartHostWiFi(ctx, store, nil, time.Hour, scanner)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	source.Stop()
}

func bytesOf(size int) []byte { return make([]byte, size) }

func TestHostWiFiScannerReportsPlatformCapability(t *testing.T) {
	scanner, err := NewHostWiFiScanner()
	if runtime.GOOS == "windows" {
		if err != nil || scanner == nil {
			t.Fatalf("Windows scanner unavailable: scanner=%v error=%v", scanner, err)
		}
		return
	}
	if scanner != nil || !errors.Is(err, ErrWiFiUnsupported) {
		t.Fatalf("non-Windows scanner result = %v, %v", scanner, err)
	}
}
