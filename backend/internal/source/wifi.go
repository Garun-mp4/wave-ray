package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/state"
)

const (
	maxWiFiScanOutput = 1 << 20
	maxWiFiBSSIDs     = 32
	minWiFiScanPeriod = 5 * time.Second
	wifiScanTimeout   = 8 * time.Second
)

var (
	ErrWiFiUnsupported = errors.New("host Wi-Fi scanning is supported only on Windows in this Go backend")
	ErrWiFiOutputLimit = errors.New("host Wi-Fi command output exceeded the configured limit")
)

// BSSIDObservation is a host Wi-Fi scan result. It contains no packet-level
// CSI: amplitude and phase derived from it are explicitly heuristic values.
type BSSIDObservation struct {
	BSSID      string
	SSID       string
	SignalPct  float64
	RSSIDBm    float64
	Channel    uint8
	RadioType  string
	ObservedAt time.Time
}

// ParseNetshBSSIDOutput parses the English key/value format emitted by
// `netsh wlan show networks mode=bssid`. Unknown/localized lines are ignored,
// matching the Rust adapter's supported format while bounding input work.
func ParseNetshBSSIDOutput(output string) ([]BSSIDObservation, error) {
	if len(output) > maxWiFiScanOutput {
		return nil, ErrWiFiOutputLimit
	}
	observedAt := time.Now()
	var observations []BSSIDObservation
	ssid := ""
	var current *BSSIDObservation
	flush := func() {
		if current != nil && current.BSSID != "" {
			if current.SignalPct < 0 || current.SignalPct > 100 {
				current.SignalPct = 0
			}
			current.RSSIDBm = current.SignalPct/2 - 100
			if current.Channel != 0 && current.RadioType == "" {
				current.RadioType = "unknown"
			}
			observations = append(observations, *current)
		}
		current = nil
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		key, value, ok := splitNetshKV(line)
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		switch {
		case strings.HasPrefix(upper, "SSID") && !strings.HasPrefix(upper, "BSSID"):
			flush()
			ssid = value
		case strings.HasPrefix(upper, "BSSID"):
			flush()
			mac, err := net.ParseMAC(value)
			if err == nil && len(mac) == 6 {
				current = &BSSIDObservation{BSSID: strings.ToLower(mac.String()), SSID: ssid, ObservedAt: observedAt}
			}
		case current == nil:
			continue
		case strings.HasPrefix(upper, "SIGNAL"):
			pct, err := parseNetshPercent(value)
			if err == nil {
				current.SignalPct = pct
			}
		case strings.HasPrefix(upper, "CHANNEL"):
			channel, err := strconv.ParseUint(strings.TrimSpace(value), 10, 8)
			if err == nil {
				current.Channel = uint8(channel)
			}
		case strings.HasPrefix(upper, "RADIO TYPE"):
			current.RadioType = normalizeRadioType(value)
		}
	}
	flush()
	if len(observations) > maxWiFiBSSIDs {
		observations = observations[:maxWiFiBSSIDs]
	}
	return observations, nil
}

// ParseNetshInterfaceOutput parses one connected-interface signal report.
func ParseNetshInterfaceOutput(output string) (*BSSIDObservation, error) {
	if len(output) > maxWiFiScanOutput {
		return nil, ErrWiFiOutputLimit
	}
	var ssid string
	var signal float64
	hasSignal := false
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := splitNetshKV(strings.TrimSpace(strings.TrimSuffix(line, "\r")))
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "SSID":
			ssid = value
		case "SIGNAL":
			parsed, err := parseNetshPercent(value)
			if err != nil {
				return nil, err
			}
			signal, hasSignal = parsed, true
		}
	}
	if !hasSignal {
		return nil, nil
	}
	now := time.Now()
	return &BSSIDObservation{SSID: ssid, SignalPct: signal, RSSIDBm: -100 + signal*0.6, ObservedAt: now}, nil
}

func splitNetshKV(line string) (string, string, bool) {
	if i := strings.Index(line, " : "); i >= 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+3:]), true
	}
	if strings.HasSuffix(line, " :") {
		return strings.TrimSpace(strings.TrimSuffix(line, " :")), "", true
	}
	return "", "", false
}

func parseNetshPercent(value string) (float64, error) {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "%"))
	pct, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
		return 0, fmt.Errorf("invalid Wi-Fi signal percentage %q", value)
	}
	return pct, nil
}

func normalizeRadioType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, mode := range []string{"802.11be", "802.11ax", "802.11ac", "802.11n"} {
		if strings.Contains(value, mode) {
			return mode
		}
	}
	return "unknown"
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

// HostWiFiScanner reads Windows WLAN scan data. The fixed executable and
// argument list avoid shell interpretation; callers cannot provide commands.
type HostWiFiScanner struct {
	run commandRunner
}

func NewHostWiFiScanner() (*HostWiFiScanner, error) {
	if runtime.GOOS != "windows" {
		return nil, ErrWiFiUnsupported
	}
	return &HostWiFiScanner{run: runNetsh}, nil
}

func (s *HostWiFiScanner) Scan(ctx context.Context) ([]BSSIDObservation, error) {
	if s == nil || s.run == nil {
		return nil, errors.New("host Wi-Fi scanner is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("host Wi-Fi scan requires a context")
	}
	commandCtx, cancel := context.WithTimeout(ctx, wifiScanTimeout)
	defer cancel()
	output, err := s.run(commandCtx, "wlan", "show", "networks", "mode=bssid")
	if err == nil {
		observations, parseErr := ParseNetshBSSIDOutput(string(output))
		if parseErr != nil {
			return nil, parseErr
		}
		if len(observations) != 0 {
			return observations, nil
		}
	}
	// Retain parity with the supported single-link fallback when Windows has a
	// connected WLAN but the driver returns no BSSID scan cache.
	fallbackOutput, fallbackErr := s.run(commandCtx, "wlan", "show", "interfaces")
	if fallbackErr != nil {
		if err != nil {
			return nil, fmt.Errorf("netsh BSSID scan failed: %w", err)
		}
		return nil, fmt.Errorf("netsh interface scan failed: %w", fallbackErr)
	}
	fallback, parseErr := ParseNetshInterfaceOutput(string(fallbackOutput))
	if parseErr != nil {
		return nil, parseErr
	}
	if fallback == nil {
		return []BSSIDObservation{}, nil
	}
	return []BSSIDObservation{*fallback}, nil
}

func runNetsh(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "netsh", append([]string{command}, args...)...)
	cmd.WaitDelay = 2 * time.Second
	stdout, stderr := &boundedBuffer{}, &boundedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, ErrWiFiOutputLimit
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, message)
	}
	return bytes.Clone(stdout.Bytes()), nil
}

type boundedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := maxWiFiScanOutput - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.exceeded = true
		_, _ = b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// WiFiSource owns the periodic host scanner goroutine and stops it on cancel.
type WiFiSource struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   chan struct{}
}

// StartHostWiFi starts the host scanner. Empty scans are normal while the
// adapter is disconnected; explicit `wifi` mode remains offline and never
// silently changes over to simulated data.
func StartHostWiFi(parent context.Context, store *state.Store, automatic *Automatic, interval time.Duration, scanner *HostWiFiScanner) (*WiFiSource, error) {
	if parent == nil || store == nil || scanner == nil {
		return nil, errors.New("host Wi-Fi source requires context, state store, and scanner")
	}
	if interval < minWiFiScanPeriod {
		interval = minWiFiScanPeriod
	}
	ctx, cancel := context.WithCancel(parent)
	source := &WiFiSource{cancel: cancel, done: make(chan struct{}), once: make(chan struct{})}
	go source.run(ctx, store, automatic, interval, scanner)
	return source, nil
}

func (s *WiFiSource) run(ctx context.Context, store *state.Store, automatic *Automatic, interval time.Duration, scanner *HostWiFiScanner) {
	defer close(s.done)
	defer close(s.once)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		observations, err := scanner.Scan(ctx)
		if err == nil && len(observations) > 0 {
			frame, ok := wifiObservationsToFrame(observations)
			if ok {
				if automatic != nil {
					automatic.IngestWiFi(frame)
				} else {
					store.IngestCSI(frame)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *WiFiSource) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

func wifiObservationsToFrame(observations []BSSIDObservation) (protocol.CSIFrame, bool) {
	valid := make([]BSSIDObservation, 0, min(len(observations), maxWiFiBSSIDs))
	seen := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		if len(valid) == maxWiFiBSSIDs {
			break
		}
		if observation.SignalPct < 0 || observation.SignalPct > 100 || math.IsNaN(observation.SignalPct) || math.IsInf(observation.SignalPct, 0) {
			continue
		}
		if observation.RSSIDBm == 0 {
			observation.RSSIDBm = observation.SignalPct/2 - 100
		}
		if observation.BSSID != "" {
			if _, duplicate := seen[observation.BSSID]; duplicate {
				continue
			}
			seen[observation.BSSID] = struct{}{}
		}
		valid = append(valid, observation)
	}
	if len(valid) == 0 {
		return protocol.CSIFrame{}, false
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].BSSID < valid[j].BSSID })
	amplitude := make([]float64, len(valid))
	phase := make([]float64, len(valid))
	var rssiSum float64
	var frequency uint16
	for i, observation := range valid {
		amplitude[i] = math.Pow(10, (observation.RSSIDBm+100)/20)
		phase[i] = (float64(observation.Channel) / 48) * math.Pi
		rssiSum += observation.RSSIDBm
		if frequency == 0 {
			frequency = channelFrequencyMHz(observation.Channel)
		}
	}
	return protocol.CSIFrame{
		Source: "wifi", InputMode: "rssi_derived", NodeID: 0, Antennas: 1, Subcarriers: uint16(len(valid)),
		FrequencyMHz: frequency, RSSIDBm: int8(math.Round(rssiSum / float64(len(valid)))),
		NoiseFloor: -90, Amplitude: amplitude, Phase: phase,
	}, true
}

func channelFrequencyMHz(channel uint8) uint16 {
	switch {
	case channel >= 1 && channel <= 13:
		return uint16(2407 + 5*int(channel))
	case channel == 14:
		return 2484
	case channel >= 32 && channel <= 177:
		return uint16(5000 + 5*int(channel))
	case channel > 0:
		return uint16(5950 + 5*int(channel))
	default:
		return 0
	}
}
