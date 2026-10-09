package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ruvnet/ruview/backend/internal/activity"
	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/httpapi"
	"github.com/ruvnet/ruview/backend/internal/ingest"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/oauth"
	posemodel "github.com/ruvnet/ruview/backend/internal/pose"
	"github.com/ruvnet/ruview/backend/internal/recording"
	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/source"
	"github.com/ruvnet/ruview/backend/internal/state"
	"github.com/ruvnet/ruview/backend/internal/telemetry"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("RuView backend stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	udpAllowlist, err := security.NewUDPAllowlist(cfg.UDPAllow)
	if err != nil {
		return err
	}
	hostAllowlist, err := security.NewHostAllowlist(cfg.AllowedHosts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if info, err := os.Stat(cfg.DataDir); err != nil || !info.IsDir() {
		return fmt.Errorf("data-dir must be a directory: %s", cfg.DataDir)
	}

	previousLogger := slog.Default()
	logger, shutdownTelemetry, telemetryEnabled, telemetryErr := telemetry.NewLogger(os.Stdout, os.Getenv)
	slog.SetDefault(logger)
	defer func() {
		if telemetryErr != nil {
			logger.Warn("OTLP log export unavailable; local logging remains active", "reason", "exporter_initialization_failed")
		}
		if telemetryEnabled && shutdownTelemetry != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := shutdownTelemetry(shutdownCtx); err != nil {
				logger.Warn("OTLP log export did not flush cleanly", "reason", "exporter_shutdown_failed")
			}
			cancel()
		}
		slog.SetDefault(previousLogger)
	}()
	if telemetryEnabled {
		logger.Info("OTLP log export enabled", "service", telemetry.ServiceName)
	}
	hub := ws.NewHub()
	store := state.New(cfg.Source, hub)
	if cfg.PoseModelPath != "" {
		poseModel, loadErr := posemodel.Load(cfg.PoseModelPath)
		if loadErr != nil {
			return fmt.Errorf("load configured pose model: %w", loadErr)
		}
		var poseNodeID *uint8
		if cfg.PoseNodeID >= 0 {
			value := uint8(cfg.PoseNodeID)
			poseNodeID = &value
		}
		if err := store.SetPoseModel(poseModel, poseNodeID); err != nil {
			return fmt.Errorf("configure pose model: %w", err)
		}
		logger.Warn("experimental pose_v1 model loaded; image pose is not an accepted person detection or metric localization", "model_sha256", poseModel.Info().ArtifactSHA256, "node_id", cfg.PoseNodeID)
	} else {
		logger.Info("experimental pose inference is disabled; set RUVIEW_POSE_MODEL_PATH to opt in")
	}
	if err := store.SetNodePositions(cfg.NodePositions); err != nil {
		return fmt.Errorf("node positions: %w", err)
	}
	if len(cfg.NodePositions) > 0 {
		logger.Info("spatial fusion node positions configured", "nodes", len(cfg.NodePositions))
	}
	adaptiveModelPath := filepath.Join(cfg.DataDir, "adaptive_model.json")
	if adaptiveModel, loadErr := adaptive.Load(adaptiveModelPath); loadErr == nil {
		if err := store.SetAdaptiveModel(adaptiveModel); err != nil {
			logger.Warn("could not activate adaptive classifier; using signal heuristics", "error", err)
		} else {
			logger.Info("adaptive classifier loaded", "trained_frames", adaptiveModel.TrainedFrames, "version", adaptiveModel.Version)
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		logger.Warn("could not load adaptive classifier; using signal heuristics", "error", loadErr)
	}
	if cfg.ActivityStatePath != "" {
		floors, loadErr := activity.LoadFloors(cfg.ActivityStatePath)
		if loadErr != nil {
			logger.Warn("could not load MediaTek activity floors; starting a fresh baseline", "error", loadErr)
		} else {
			store.SeedMediaTekActivityFloors(floors)
			logger.Info("MediaTek activity floor state loaded", "device_count", len(floors))
		}
	}
	models, err := model.NewRegistry(cfg.ModelsDir)
	if err != nil {
		return fmt.Errorf("model registry: %w", err)
	}
	store.SetModelRegistry(models)
	recorder, err := recording.New(cfg.DataDir, hub, cfg.PrivacyMode)
	if err != nil {
		return fmt.Errorf("recording manager: %w", err)
	}
	var oauthService *oauth.Service
	if cfg.OAuthIssuer != "" {
		publicBaseURL := cfg.OAuthPublicBaseURL
		if publicBaseURL == "" {
			publicBaseURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort)
		}
		oauthService, err = oauth.New(context.Background(), oauth.Config{
			Issuer: cfg.OAuthIssuer, JWKSURL: cfg.OAuthJWKSURL,
			AllowedClientIDs: cfg.OAuthClientIDs, PublicBaseURL: publicBaseURL,
			SessionSecret: cfg.SessionSecret, DataDir: cfg.DataDir,
		})
		if err != nil {
			return fmt.Errorf("OAuth initialization: %w", err)
		}
		for _, clientID := range cfg.OAuthClientIDs {
			if clientID == "*" {
				logger.Warn("OAuth client audience restriction is disabled; tokens for any client_id may be accepted")
				break
			}
		}
		logger.Info("Cognitum OAuth enabled", "issuer", oauthService.Issuer(), "client_id", oauthService.PrimaryClientID())
	}
	api := httpapi.New(cfg, store, security.NewTicketStore(), hostAllowlist, models, recorder, logger)
	defer api.Close()
	api.OAuth = oauthService
	handler := api.Handler()
	servers := []*http.Server{newHTTPServer(cfg.HTTPAddr(), handler)}
	if cfg.WSPort != 0 && cfg.WSPort != cfg.HTTPPort {
		servers = append(servers, newHTTPServer(cfg.WSAddr(), handler))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := api.Start(ctx); err != nil {
		return fmt.Errorf("start background integrations: %w", err)
	}
	refreshDone := make(chan struct{})
	go runStateRefresh(ctx, store, time.Duration(cfg.TickMS)*time.Millisecond, refreshDone)
	var automatic *source.Automatic
	if cfg.Source == "auto" {
		automatic, err = source.StartAutomatic(ctx, store, time.Duration(cfg.TickMS)*time.Millisecond)
		if err != nil {
			return err
		}
		defer automatic.Stop()
		logger.Warn("no source probe is available yet; serving explicitly labeled simulation until the first valid ESP32 packet")
	}
	var receiver *ingest.Receiver
	if !cfg.DisableUDP && cfg.Source != "simulate" && cfg.Source != "wifi" {
		var sink ingest.Sink = store
		if automatic != nil {
			sink = automatic
		}
		receiver, err = ingest.ListenUDP(ctx, cfg.UDPAddr(), udpAllowlist, sink, logger)
		if err != nil {
			return err
		}
		api.Ingress = receiver
		logger.Info("CSI UDP listener ready", "address", receiver.Addr().String(), "allowlist_active", udpAllowlist.Active())
	} else if cfg.Source == "simulate" {
		logger.Warn("synthetic source explicitly enabled; output is not live sensor data")
		source.StartSimulation(ctx, store, time.Duration(cfg.TickMS)*time.Millisecond)
	}
	if cfg.Source == "wifi" || cfg.Source == "auto" {
		scanner, scanErr := source.NewHostWiFiScanner()
		if scanErr != nil {
			if cfg.Source == "wifi" {
				return fmt.Errorf("host Wi-Fi source: %w", scanErr)
			}
			logger.Info("host Wi-Fi probing is unavailable on this platform; auto mode keeps UDP and labeled fallback", "reason", scanErr.Error())
		} else {
			wifiSource, startErr := source.StartHostWiFi(ctx, store, automatic, time.Duration(cfg.TickMS)*time.Millisecond, scanner)
			if startErr != nil {
				if cfg.Source == "wifi" {
					return fmt.Errorf("start host Wi-Fi source: %w", startErr)
				}
				logger.Warn("host Wi-Fi source could not start", "error", startErr)
			} else {
				defer wifiSource.Stop()
				logger.Info("host Wi-Fi RSSI source started", "minimum_scan_interval", "5s")
			}
		}
	}
	var stopActivityPersistence context.CancelFunc
	var activityPersistenceDone chan struct{}
	if cfg.ActivityStatePath != "" {
		var persistenceCtx context.Context
		persistenceCtx, stopActivityPersistence = context.WithCancel(context.Background())
		activityPersistenceDone = make(chan struct{})
		go func() {
			defer close(activityPersistenceDone)
			runActivityPersistence(persistenceCtx, store, cfg.ActivityStatePath, logger)
		}()
	}

	errorsCh := make(chan error, len(servers))
	for _, server := range servers {
		server := server
		go func() {
			logger.Info("HTTP/REST/WebSocket listener ready", "address", server.Addr)
			if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errorsCh <- fmt.Errorf("serve %s: %w", server.Addr, serveErr)
			}
		}()
	}

	var runErr error
	var receiverDone <-chan struct{}
	if receiver != nil {
		receiverDone = receiver.Done()
	}
	select {
	case <-ctx.Done():
	case runErr = <-errorsCh:
	case <-receiverDone:
		if ctx.Err() == nil && receiver.Err() != nil {
			receiveErr := receiver.Err()
			runErr = fmt.Errorf("CSI UDP receiver stopped: %w", receiveErr)
		} else if ctx.Err() == nil {
			runErr = errors.New("CSI UDP receiver stopped unexpectedly")
		}
	}
	stop()
	<-refreshDone
	if automatic != nil {
		automatic.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown timed out", "address", server.Addr, "error", err)
			_ = server.Close()
		}
	}
	if receiver != nil {
		_ = receiver.Close()
		receiver.Wait()
	}
	if stopActivityPersistence != nil {
		stopActivityPersistence()
		<-activityPersistenceDone
	}
	hub.Close()
	return runErr
}

func runStateRefresh(ctx context.Context, store *state.Store, interval time.Duration, done chan<- struct{}) {
	defer close(done)
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			store.Refresh()
		}
	}
}

func runActivityPersistence(ctx context.Context, store *state.Store, path string, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	persist := func() {
		if err := activity.WriteFloors(path, store.MediaTekActivityFloors()); err != nil {
			logger.Warn("could not persist MediaTek activity floors", "error", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			persist()
			return
		case <-ticker.C:
			persist()
		}
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
