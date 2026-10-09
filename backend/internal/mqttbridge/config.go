package mqttbridge

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDiscoveryPrefix = "homeassistant"
	DefaultRefreshInterval = 10 * time.Minute
	DefaultNodeStaleAfter  = 10 * time.Second
	clientIDFile           = "mqtt_client_id"
)

var ErrConfig = errors.New("invalid MQTT integration configuration")

// Config contains the broker connection and Home Assistant discovery settings.
// Password is resolved from an environment variable and is never surfaced in
// status responses or logs.
type Config struct {
	BrokerURL       string
	Host            string
	Port            uint16
	TLS             *tls.Config
	Username        string
	Password        string
	ClientID        string
	Prefix          string
	RefreshInterval time.Duration
	RSSIInterval    time.Duration
}

func ConfigFromEnv(getenv func(string) string, dataDir string) (Config, bool, error) {
	if getenv == nil {
		return Config{}, false, fmt.Errorf("%w: environment reader is nil", ErrConfig)
	}
	enabled, err := envBool(getenv("RUVIEW_MQTT"), false)
	if err != nil {
		return Config{}, false, err
	}
	if !enabled {
		return Config{}, false, nil
	}

	host := envOr(getenv, "RUVIEW_MQTT_HOST", "localhost")
	tlsEnabled, err := envBool(getenv("RUVIEW_MQTT_TLS"), false)
	if err != nil {
		return Config{}, false, err
	}
	defaultPort := uint64(1883)
	if tlsEnabled {
		defaultPort = 8883
	}
	port := defaultPort
	if value := strings.TrimSpace(getenv("RUVIEW_MQTT_PORT")); value != "" {
		port, err = strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_PORT must be 1..65535", ErrConfig)
		}
	}
	prefix := envOr(getenv, "RUVIEW_MQTT_PREFIX", DefaultDiscoveryPrefix)
	if prefix != DefaultDiscoveryPrefix {
		return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_PREFIX must be %q", ErrConfig, DefaultDiscoveryPrefix)
	}

	username := strings.TrimSpace(getenv("RUVIEW_MQTT_USERNAME"))
	passwordEnv := envOr(getenv, "RUVIEW_MQTT_PASSWORD_ENV", "MQTT_PASSWORD")
	if !validEnvName(passwordEnv) {
		return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_PASSWORD_ENV must name an environment variable", ErrConfig)
	}
	password := getenv(passwordEnv)
	if (username == "") != (password == "") {
		return Config{}, false, fmt.Errorf("%w: broker username and password must be configured together", ErrConfig)
	}
	if strings.ContainsAny(username, "\r\n\x00") || len(username) > 256 || len(password) > 4096 || strings.ContainsAny(password, "\r\n\x00") {
		return Config{}, false, fmt.Errorf("%w: broker credentials have invalid length or characters", ErrConfig)
	}

	clientID := strings.TrimSpace(getenv("RUVIEW_MQTT_CLIENT_ID"))
	if clientID == "" {
		clientID, err = loadOrCreateClientID(dataDir)
		if err != nil {
			return Config{}, false, fmt.Errorf("%w: persist MQTT client id: %v", ErrConfig, err)
		}
	}
	if !validClientID(clientID) {
		return Config{}, false, fmt.Errorf("%w: MQTT client ID must be 1..64 ASCII letters, digits, '_' or '-'", ErrConfig)
	}

	refresh := DefaultRefreshInterval
	if value := strings.TrimSpace(getenv("RUVIEW_MQTT_REFRESH_SECS")); value != "" {
		seconds, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil || seconds == 0 || seconds > 86400 {
			return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_REFRESH_SECS must be 1..86400", ErrConfig)
		}
		refresh = time.Duration(seconds) * time.Second
	}
	rssiInterval := 10 * time.Second
	if value := strings.TrimSpace(getenv("RUVIEW_MQTT_RSSI_SECS")); value != "" {
		seconds, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil || seconds == 0 || seconds > 3600 {
			return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_RSSI_SECS must be 1..3600", ErrConfig)
		}
		rssiInterval = time.Duration(seconds) * time.Second
	}

	var tlsConfig *tls.Config
	if tlsEnabled {
		tlsConfig, err = loadTLSConfig(getenv)
		if err != nil {
			return Config{}, false, err
		}
	}
	if !validHost(host) {
		return Config{}, false, fmt.Errorf("%w: RUVIEW_MQTT_HOST is not a valid hostname or IP address", ErrConfig)
	}
	return Config{
		BrokerURL: brokerURL(host, uint16(port), tlsEnabled), Host: host, Port: uint16(port),
		TLS: tlsConfig, Username: username, Password: password, ClientID: clientID,
		Prefix: prefix, RefreshInterval: refresh, RSSIInterval: rssiInterval,
	}, true, nil
}

func loadTLSConfig(getenv func(string) string) (*tls.Config, error) {
	caFile := strings.TrimSpace(getenv("RUVIEW_MQTT_CA_FILE"))
	certFile := strings.TrimSpace(getenv("RUVIEW_MQTT_CLIENT_CERT"))
	keyFile := strings.TrimSpace(getenv("RUVIEW_MQTT_CLIENT_KEY"))
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("%w: RUVIEW_MQTT_CLIENT_CERT and RUVIEW_MQTT_CLIENT_KEY must be set together", ErrConfig)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(filepath.Clean(caFile))
		if err != nil {
			return nil, fmt.Errorf("%w: read MQTT CA file: %v", ErrConfig, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: MQTT CA file contains no certificates", ErrConfig)
		}
		config.RootCAs = pool
	}
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(filepath.Clean(certFile), filepath.Clean(keyFile))
		if err != nil {
			return nil, fmt.Errorf("%w: load MQTT client certificate: %v", ErrConfig, err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

func brokerURL(host string, port uint16, tlsEnabled bool) string {
	scheme := "tcp"
	if tlsEnabled {
		scheme = "ssl"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(int(port)))
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\?#@\r\n\x00") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return true
}

func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validEnvName(name string) bool {
	if len(name) == 0 || len(name) > 128 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for _, char := range name {
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return true
}

func validClientID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, char := range id {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func envOr(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(value string, fallback bool) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%w: expected a boolean environment value", ErrConfig)
	}
}

func loadOrCreateClientID(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", errors.New("a data directory is required for a persistent client ID")
	}
	path := filepath.Join(dataDir, clientIDFile)
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if validClientID(id) {
			return id, nil
		}
		return "", errors.New("stored MQTT client ID is malformed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := fmt.Sprintf("ruview-%x", random[:])
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", readErr
			}
			id = strings.TrimSpace(string(data))
			if validClientID(id) {
				return id, nil
			}
			return "", errors.New("stored MQTT client ID is malformed")
		}
		return "", err
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}
