// Package discovery advertises the local HTTP control plane over DNS-SD.
package discovery

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	mdns "github.com/cybergarage/go-mdns/mdns"
)

const (
	ServiceType = "_ruview._tcp"
	Schema      = "ruview.installation.v1"
)

// Settings describes the non-secret metadata published on the local network.
type Settings struct {
	Disabled         bool
	BindAddress      string
	HTTPPort         uint16
	InstallationID   string
	InstallationName string
}

// Advertiser owns the mDNS responder and withdraws its records at shutdown.
type Advertiser struct {
	server  *mdns.Server
	service *mdns.LocalService
}

// ShouldAdvertise follows the server contract: disable explicitly or when
// the HTTP listener is loopback-only. Wildcard and LAN binds are discoverable.
func ShouldAdvertise(settings Settings) bool {
	if settings.Disabled {
		return false
	}
	bindIP := net.ParseIP(strings.Trim(settings.BindAddress, "[]"))
	return bindIP == nil || !bindIP.IsLoopback()
}

// BuildService validates a DNS-SD record without opening network sockets.
// TXT contains only a schema version, transport flag, and operator-provided
// installation routing hint; it never includes sensor or credential data.
func BuildService(name, installationID string, port uint16) (*mdns.LocalService, error) {
	if port == 0 {
		return nil, errors.New("mDNS service port is required")
	}
	if strings.TrimSpace(name) == "" || len(name) > 63 || strings.ContainsAny(name, "\r\n\x00") {
		return nil, errors.New("mDNS service name is invalid")
	}
	if strings.TrimSpace(installationID) == "" || len(installationID) > 128 || strings.ContainsAny(installationID, "\r\n\x00") {
		return nil, errors.New("mDNS installation identifier is invalid")
	}
	host := discoveryHostname(installationID)
	service := &mdns.LocalService{
		Instance: name,
		Service:  ServiceType,
		Domain:   "local",
		Host:     host,
		Port:     int(port),
		TXT:      []string{"schema=" + Schema, "tls=0", "installation=" + installationID},
	}
	if err := service.Validate(); err != nil {
		return nil, errors.New("mDNS service record is invalid")
	}
	return service, nil
}

// Start starts the responder when discovery is enabled for a non-loopback
// listener. Network discovery failures are recoverable and reported to the
// caller so the HTTP server can continue in manual-origin mode.
func Start(ctx context.Context, settings Settings) (*Advertiser, error) {
	if !ShouldAdvertise(settings) {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	service, err := BuildService(settings.InstallationName, settings.InstallationID, settings.HTTPPort)
	if err != nil {
		return nil, err
	}
	server := mdns.NewServer()
	if err := server.Start(); err != nil {
		return nil, errors.New("mDNS responder could not start")
	}
	registerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := server.Register(registerCtx, service); err != nil {
		_ = server.Stop()
		return nil, errors.New("mDNS service could not be registered")
	}
	return &Advertiser{server: server, service: service}, nil
}

// Close sends DNS-SD goodbye records and closes the responder sockets.
func (a *Advertiser) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	if a.service != nil {
		if err := a.server.Deregister(a.service); err != nil {
			_ = a.server.Stop()
			return err
		}
	}
	return a.server.Stop()
}

func discoveryHostname(raw string) string {
	var label strings.Builder
	label.Grow(40)
	for _, character := range strings.ToLower(raw) {
		if label.Len() >= 40 {
			break
		}
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			label.WriteRune(character)
		} else if (character == '-' || character == '_' || character == ' ') && (label.Len() == 0 || !strings.HasSuffix(label.String(), "-")) {
			label.WriteByte('-')
		}
	}
	safe := strings.Trim(label.String(), "-")
	if safe == "" {
		safe = "installation"
	}
	return "ruview-" + safe
}
