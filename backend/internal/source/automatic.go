package source

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/protocol"
	"github.com/ruvnet/ruview/backend/internal/radar"
	"github.com/ruvnet/ruview/backend/internal/state"
)

// Automatic waits without producing data until a real source arrives.
type Automatic struct {
	store         *state.Store
	cancel        context.CancelFunc
	done          chan struct{}
	transition    chan struct{}
	mu            sync.Mutex
	state         automaticState
	currentSource string
}

type automaticState uint8

const (
	automaticRunning automaticState = iota
	automaticPromoting
	automaticPromoted
	automaticStopped
)

func StartAutomatic(parent context.Context, store *state.Store, interval time.Duration) (*Automatic, error) {
	if parent == nil || store == nil {
		return nil, errors.New("automatic source requires a context and state store")
	}
	waitCtx, cancel := context.WithCancel(parent)
	automatic := &Automatic{
		store: store, cancel: cancel, done: make(chan struct{}),
		transition: make(chan struct{}), state: automaticRunning,
	}
	store.SetSource("auto")
	go func() {
		defer close(automatic.done)
		<-waitCtx.Done()
	}()
	return automatic, nil
}

func (a *Automatic) IngestCSI(frame protocol.CSIFrame) {
	a.applySource("esp32", func() { a.store.IngestCSI(frame) })
}

// IngestWiFi accepts host RSSI observations without representing them as ESP32
// CSI or allowing them to overwrite a fresh ESP32 stream.
func (a *Automatic) IngestWiFi(frame protocol.CSIFrame) {
	frame.Source = "wifi"
	frame.InputMode = "rssi_derived"
	a.applySource("wifi", func() { a.store.IngestCSI(frame) })
}

func (a *Automatic) IngestVendorCSI(snapshot csivendor.Snapshot) {
	a.applySource(snapshot.Source, func() { a.store.IngestVendorCSI(snapshot) })
}

// IngestRadar stores the payload-free radar summary without changing the
// active Wi-Fi sensing source.
func (a *Automatic) IngestRadar(snapshot radar.Snapshot) {
	a.store.IngestRadar(snapshot)
}

func (a *Automatic) IngestVitals(packet protocol.VitalsPacket) {
	a.applySource("esp32", func() { a.store.IngestVitals(packet) })
}

func (a *Automatic) IngestEdgeFusedVitals(packet protocol.EdgeFusedVitalsPacket) {
	a.applySource("esp32", func() { a.store.IngestEdgeFusedVitals(packet) })
}

// WASM output is auxiliary module telemetry and does not by itself prove that
// the sensing source is active or promote the automatic source selection.
func (a *Automatic) IngestWASMOutput(packet protocol.WASMOutputPacket) {
	a.store.IngestWASMOutput(packet)
}

func (a *Automatic) IngestFeatureState(packet protocol.FeatureStatePacket) {
	a.applySource("esp32", func() { a.store.IngestFeatureState(packet) })
}

// IngestSync records mesh clock metadata without promoting a synthetic source:
// sync beacons are not sensing frames and do not prove that CSI is arriving.
func (a *Automatic) IngestSync(packet protocol.SyncPacket) {
	a.store.IngestSync(packet)
}

func (a *Automatic) Stop() {
	a.mu.Lock()
	if a.state == automaticRunning {
		a.state = automaticStopped
		a.cancel()
		close(a.transition)
	}
	transition := a.transition
	a.mu.Unlock()
	a.cancel()
	<-a.done
	<-transition
}

func (a *Automatic) promote(source string) {
	a.mu.Lock()
	switch a.state {
	case automaticRunning:
		a.state = automaticPromoting
		a.mu.Unlock()
		a.cancel()
		<-a.done
		a.store.ClearNodes()
		a.store.SetSource(source)
		a.mu.Lock()
		a.currentSource = source
		a.state = automaticPromoted
		close(a.transition)
		a.mu.Unlock()
		return
	case automaticPromoting:
		transition := a.transition
		a.mu.Unlock()
		<-transition
		return
	default:
		a.mu.Unlock()
		return
	}
}

func (a *Automatic) applySource(source string, ingest func()) {
	a.promote(source)
	a.mu.Lock()
	defer a.mu.Unlock()
	if source == "wifi" && a.currentSource == "esp32" {
		if health, _ := a.store.Health()["source"].(string); health == "esp32" {
			return
		}
	}
	if source == "esp32" && a.currentSource == "wifi" {
		a.store.ClearNodes()
		a.store.SetSource("esp32")
	}
	a.currentSource = source
	ingest()
}
