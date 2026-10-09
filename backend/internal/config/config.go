package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultEdgeRegistryURL       = "https://storage.googleapis.com/cognitum-apps/app-registry.json"
	DefaultEdgeRegistryTTLSecond = uint64(3600)
	MaxEdgeRegistryTTLSeconds    = uint64((1<<63 - 1) / 1_000_000_000)
)

// Config contains values that affect process lifecycle and network exposure.
// Secrets are loaded only from the environment, never from command-line flags.
type Config struct {
	BindAddr             string
	HTTPPort             uint16
	WSPort               uint16
	UDPBindAddr          string
	UDPPort              uint16
	DisableUDP           bool
	UDPAllow             []string
	UDPInsecureLAN       bool
	AllowedHosts         []string
	Source               string
	NodePositions        map[uint8][3]float64
	TickMS               uint64
	UIPath               string
	DataDir              string
	ModelsDir            string
	PoseModelPath        string
	PoseNodeID           int
	ActivityStatePath    string
	PrivacyMode          bool
	APIToken             string
	OAuthIssuer          string
	OAuthJWKSURL         string
	OAuthClientIDs       []string
	OAuthPublicBaseURL   string
	SessionSecret        string
	AllowUnauthenticated bool
	NoEdgeRegistry       bool
	EdgeRegistryURL      string
	EdgeRegistryTTL      uint64
}

func defaults(getenv func(string) string) (Config, error) {
	bindAddr := envOr(getenv, "RUVIEW_BIND_ADDR", envOr(getenv, "SENSING_BIND_ADDR", "127.0.0.1"))
	udpBind := envOr(getenv, "RUVIEW_UDP_BIND", envOr(getenv, "SENSING_UDP_BIND_ADDR", bindAddr))
	source := envOr(getenv, "CSI_SOURCE", "auto")
	if strings.EqualFold(source, "simulated") {
		source = "simulate"
	}
	privacyMode, err := parseBool(getenv("RUVIEW_PRIVACY_MODE"), "RUVIEW_PRIVACY_MODE")
	if err != nil {
		return Config{}, err
	}
	allowUnauthenticated, err := parseBool(getenv("RUVIEW_ALLOW_UNAUTHENTICATED"), "RUVIEW_ALLOW_UNAUTHENTICATED")
	if err != nil {
		return Config{}, err
	}
	udpInsecureLAN, err := parseBool(getenv("RUVIEW_UDP_INSECURE_LAN"), "RUVIEW_UDP_INSECURE_LAN")
	if err != nil {
		return Config{}, err
	}
	noEdgeRegistry, err := parseBool(getenv("RUVIEW_NO_EDGE_REGISTRY"), "RUVIEW_NO_EDGE_REGISTRY")
	if err != nil {
		return Config{}, err
	}
	poseNodeID := -1
	if value := strings.TrimSpace(getenv("RUVIEW_POSE_NODE_ID")); value != "" {
		if value != "-1" {
			parsed, parseErr := strconv.ParseUint(value, 10, 8)
			if parseErr != nil {
				return Config{}, fmt.Errorf("RUVIEW_POSE_NODE_ID must be -1 or an integer between 0 and 255: %w", parseErr)
			}
			poseNodeID = int(parsed)
		}
	}
	ttlValue := strconv.FormatUint(DefaultEdgeRegistryTTLSecond, 10)
	if value := strings.TrimSpace(getenv("RUVIEW_EDGE_REGISTRY_TTL_SECS")); value != "" {
		ttlValue = value
	}
	edgeRegistryTTL, err := strconv.ParseUint(ttlValue, 10, 64)
	if err != nil {
		return Config{}, fmt.Errorf("RUVIEW_EDGE_REGISTRY_TTL_SECS must be a positive integer: %w", err)
	}
	return Config{
		BindAddr:             bindAddr,
		HTTPPort:             8080,
		WSPort:               8765,
		UDPBindAddr:          udpBind,
		UDPPort:              5005,
		Source:               source,
		TickMS:               100,
		UIPath:               "./ui",
		DataDir:              envOr(getenv, "RUVIEW_DATA_DIR", "./data"),
		ModelsDir:            envOr(getenv, "MODELS_DIR", "./data/models"),
		PoseModelPath:        strings.TrimSpace(getenv("RUVIEW_POSE_MODEL_PATH")),
		PoseNodeID:           poseNodeID,
		ActivityStatePath:    strings.TrimSpace(getenv("RUVIEW_ACTIVITY_STATE")),
		PrivacyMode:          privacyMode,
		APIToken:             strings.TrimSpace(getenv("RUVIEW_API_TOKEN")),
		OAuthIssuer:          strings.TrimSpace(getenv("RUVIEW_OAUTH_ISSUER")),
		OAuthJWKSURL:         strings.TrimSpace(getenv("RUVIEW_OAUTH_JWKS_URL")),
		OAuthClientIDs:       clientIDs(getenv("RUVIEW_OAUTH_CLIENT_IDS")),
		OAuthPublicBaseURL:   strings.TrimSpace(getenv("RUVIEW_PUBLIC_BASE_URL")),
		SessionSecret:        getenv("RUVIEW_SESSION_SECRET"),
		AllowUnauthenticated: allowUnauthenticated,
		UDPAllow:             splitList(envOr(getenv, "RUVIEW_UDP_ALLOW", getenv("SENSING_UDP_ALLOW"))),
		UDPInsecureLAN:       udpInsecureLAN,
		AllowedHosts:         splitList(getenv("SENSING_ALLOWED_HOSTS")),
		NoEdgeRegistry:       noEdgeRegistry,
		EdgeRegistryURL:      envOr(getenv, "RUVIEW_EDGE_REGISTRY_URL", DefaultEdgeRegistryURL),
		EdgeRegistryTTL:      edgeRegistryTTL,
	}, nil
}

func envOr(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseBool(value, name string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be one of 1/0, true/false, yes/no, or on/off (got %q)", name, value)
	}
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func clientIDs(value string) []string {
	ids := splitList(value)
	if len(ids) == 0 {
		return []string{"ruview"}
	}
	return ids
}

// Load parses the supported server flags and validates all network and path
// settings before any listener is opened.
func Load(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg, err := defaults(getenv)
	if err != nil {
		return Config{}, err
	}
	fs := flag.NewFlagSet("ruview-server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var udpAllow, allowedHosts stringList
	nodePositions := strings.TrimSpace(getenv("SENSING_NODE_POSITIONS"))
	httpPort, wsPort, udpPort := uint(cfg.HTTPPort), uint(cfg.WSPort), uint(cfg.UDPPort)
	fs.StringVar(&cfg.BindAddr, "bind-addr", cfg.BindAddr, "HTTP and WebSocket bind address")
	fs.UintVar(&httpPort, "http-port", httpPort, "HTTP and REST port")
	fs.UintVar(&wsPort, "ws-port", wsPort, "WebSocket port; 0 disables the second listener")
	fs.StringVar(&cfg.UDPBindAddr, "udp-bind-addr", cfg.UDPBindAddr, "CSI UDP bind address")
	fs.StringVar(&cfg.UDPBindAddr, "udp-bind", cfg.UDPBindAddr, "CSI UDP bind address (compatibility alias)")
	fs.UintVar(&udpPort, "udp-port", udpPort, "CSI UDP port")
	fs.BoolVar(&cfg.DisableUDP, "no-udp", false, "disable CSI UDP input")
	fs.Var(&udpAllow, "udp-allow", "allowed UDP sender IP/CIDR; may be repeated or comma-separated")
	fs.BoolVar(&cfg.UDPInsecureLAN, "udp-insecure-lan", false, "allow routable UDP bind without a source allowlist")
	fs.Var(&allowedHosts, "allowed-host", "allowed Host header; may be repeated or comma-separated")
	fs.StringVar(&cfg.Source, "source", cfg.Source, "data source: auto, wifi, esp32, or simulate")
	fs.StringVar(&nodePositions, "node-positions", nodePositions, "node coordinates as x,y,z entries or node_id:x,y,z entries separated by semicolons")
	fs.Uint64Var(&cfg.TickMS, "tick-ms", cfg.TickMS, "minimum stream update interval in milliseconds")
	fs.StringVar(&cfg.UIPath, "static-dir", cfg.UIPath, "directory containing the static UI")
	fs.StringVar(&cfg.UIPath, "ui-path", cfg.UIPath, "directory containing the static UI (compatibility alias)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "private application state directory")
	fs.StringVar(&cfg.ModelsDir, "models-dir", cfg.ModelsDir, "directory containing RVF model files")
	fs.StringVar(&cfg.PoseModelPath, "pose-model", cfg.PoseModelPath, "optional experimental pose_v1 safetensors model path")
	fs.IntVar(&cfg.PoseNodeID, "pose-node-id", cfg.PoseNodeID, "CSI node ID used for single-sensor pose inference; -1 requires exactly one active node")
	fs.StringVar(&cfg.ActivityStatePath, "activity-state", cfg.ActivityStatePath, "optional persistent MediaTek activity floor state file")
	fs.BoolVar(&cfg.PrivacyMode, "privacy-mode", cfg.PrivacyMode, "remove biometric fields from all output")
	fs.StringVar(&cfg.EdgeRegistryURL, "edge-registry-url", cfg.EdgeRegistryURL, "edge registry URL")
	fs.Uint64Var(&cfg.EdgeRegistryTTL, "edge-registry-ttl-secs", cfg.EdgeRegistryTTL, "edge registry cache TTL in seconds")
	fs.BoolVar(&cfg.NoEdgeRegistry, "no-edge-registry", cfg.NoEdgeRegistry, "disable remote edge registry lookup")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if httpPort > 65535 || wsPort > 65535 || udpPort > 65535 {
		return Config{}, errors.New("ports must be between 0 and 65535")
	}
	cfg.HTTPPort, cfg.WSPort, cfg.UDPPort = uint16(httpPort), uint16(wsPort), uint16(udpPort)
	cfg.Source = strings.ToLower(strings.TrimSpace(cfg.Source))
	if cfg.Source == "simulated" {
		cfg.Source = "simulate"
	}
	cfg.AllowedHosts = append(cfg.AllowedHosts, allowedHosts...)
	cfg.UDPAllow = append(cfg.UDPAllow, udpAllow...)
	cfg.NodePositions, err = parseNodePositions(nodePositions)
	if err != nil {
		return Config{}, fmt.Errorf("node positions: %w", err)
	}
	if len(cfg.AllowedHosts) == 0 {
		cfg.AllowedHosts = append(cfg.AllowedHosts, cfg.BindAddr, "localhost", "127.0.0.1", "[::1]")
	}
	if len(cfg.UDPAllow) == 0 {
		cfg.UDPAllow = splitList(getenv("SENSING_UDP_ALLOW"))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, splitList(value)...)
	return nil
}

func (c Config) Validate() error {
	bindIP := net.ParseIP(strings.Trim(c.BindAddr, "[]"))
	if bindIP == nil {
		return fmt.Errorf("invalid bind address %q", c.BindAddr)
	}
	udpIP := net.ParseIP(strings.Trim(c.UDPBindAddr, "[]"))
	if udpIP == nil {
		return fmt.Errorf("invalid UDP bind address %q", c.UDPBindAddr)
	}
	if c.HTTPPort == 0 {
		return errors.New("http-port must be between 1 and 65535")
	}
	if c.UDPPort == 0 && !c.DisableUDP {
		return errors.New("udp-port must be between 1 and 65535 when UDP is enabled")
	}
	if c.TickMS == 0 || c.TickMS > 60_000 {
		return errors.New("tick-ms must be between 1 and 60000")
	}
	switch strings.ToLower(c.Source) {
	case "auto", "esp32", "wifi", "simulate", "simulated":
	default:
		return fmt.Errorf("unsupported source %q (valid: auto, esp32, wifi, simulate)", c.Source)
	}
	if _, err := filepath.Abs(c.DataDir); err != nil {
		return fmt.Errorf("invalid data-dir: %w", err)
	}
	if _, err := filepath.Abs(c.ModelsDir); err != nil || strings.TrimSpace(c.ModelsDir) == "" {
		return errors.New("invalid models-dir")
	}
	if c.PoseModelPath != "" {
		if _, err := filepath.Abs(c.PoseModelPath); err != nil {
			return fmt.Errorf("invalid pose-model path: %w", err)
		}
	}
	if c.PoseNodeID < -1 || c.PoseNodeID > 255 {
		return errors.New("pose-node-id must be -1 or between 0 and 255")
	}
	if strings.TrimSpace(c.ActivityStatePath) != "" {
		if _, err := filepath.Abs(c.ActivityStatePath); err != nil {
			return fmt.Errorf("invalid activity-state path: %w", err)
		}
	}
	if !c.NoEdgeRegistry && c.EdgeRegistryURL != "" {
		parsed, err := url.Parse(c.EdgeRegistryURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("edge registry URL must be an absolute HTTP(S) URL without credentials or a fragment")
		}
		if c.EdgeRegistryTTL == 0 || c.EdgeRegistryTTL > MaxEdgeRegistryTTLSeconds {
			return errors.New("edge-registry-ttl-secs is outside the supported duration range")
		}
	}
	if c.OAuthIssuer != "" {
		if err := validateOAuthUpstreamURL(c.OAuthIssuer, "RUVIEW_OAUTH_ISSUER"); err != nil {
			return err
		}
		if c.OAuthJWKSURL != "" {
			if err := validateOAuthUpstreamURL(c.OAuthJWKSURL, "RUVIEW_OAUTH_JWKS_URL"); err != nil {
				return err
			}
		}
		if c.OAuthPublicBaseURL != "" {
			if err := validateOAuthURL(c.OAuthPublicBaseURL, "RUVIEW_PUBLIC_BASE_URL"); err != nil {
				return err
			}
		}
	} else if c.OAuthJWKSURL != "" || c.OAuthPublicBaseURL != "" {
		return errors.New("OAuth JWKS and public base URL require RUVIEW_OAUTH_ISSUER")
	}
	if !bindIP.IsLoopback() && c.APIToken == "" && c.OAuthIssuer == "" && !c.AllowUnauthenticated {
		return fmt.Errorf("refusing to serve on routable address %s with API auth off; set RUVIEW_API_TOKEN, use loopback, or explicitly set RUVIEW_ALLOW_UNAUTHENTICATED=1", bindIP)
	}
	if !udpIP.IsLoopback() && len(c.UDPAllow) == 0 && !c.UDPInsecureLAN && !c.DisableUDP {
		return fmt.Errorf("refusing to bind UDP to routable address %s without --udp-allow or --udp-insecure-lan", udpIP)
	}
	return nil
}

func validateOAuthURL(value, name string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL without credentials, query, or fragment", name)
	}
	return nil
}

func validateOAuthUpstreamURL(value, name string) error {
	if err := validateOAuthURL(value, name); err != nil {
		return err
	}
	parsed, _ := url.Parse(strings.TrimSpace(value))
	if parsed.Scheme == "https" {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("%s must use HTTPS unless it is a loopback development endpoint", name)
}

func (c Config) HTTPAddr() string {
	return net.JoinHostPort(strings.Trim(c.BindAddr, "[]"), strconv.Itoa(int(c.HTTPPort)))
}
func (c Config) WSAddr() string {
	return net.JoinHostPort(strings.Trim(c.BindAddr, "[]"), strconv.Itoa(int(c.WSPort)))
}
func (c Config) UDPAddr() string {
	return net.JoinHostPort(strings.Trim(c.UDPBindAddr, "[]"), strconv.Itoa(int(c.UDPPort)))
}

// parseNetIP is shared by tests and consumers that need strict IP parsing.
func parseNetIP(value string) (netip.Addr, error) { return netip.ParseAddr(strings.Trim(value, "[]")) }
