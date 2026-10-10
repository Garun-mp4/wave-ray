package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	websocket "github.com/coder/websocket"
	"github.com/ruvnet/ruview/backend/internal/adaptive"
	"github.com/ruvnet/ruview/backend/internal/calibration"
	"github.com/ruvnet/ruview/backend/internal/config"
	"github.com/ruvnet/ruview/backend/internal/csivendor"
	"github.com/ruvnet/ruview/backend/internal/discovery"
	"github.com/ruvnet/ruview/backend/internal/edgeregistry"
	"github.com/ruvnet/ruview/backend/internal/ingest"
	"github.com/ruvnet/ruview/backend/internal/localize"
	"github.com/ruvnet/ruview/backend/internal/model"
	"github.com/ruvnet/ruview/backend/internal/mqttbridge"
	"github.com/ruvnet/ruview/backend/internal/oauth"
	"github.com/ruvnet/ruview/backend/internal/recording"
	"github.com/ruvnet/ruview/backend/internal/rufield"
	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/state"
	"github.com/ruvnet/ruview/backend/internal/training"
	"github.com/ruvnet/ruview/backend/internal/vendor"
	stream "github.com/ruvnet/ruview/backend/internal/ws"
)

const serverVersion = "0.1.0-go"

type Server struct {
	Config          config.Config
	State           *state.Store
	Tickets         *security.TicketStore
	Hosts           *security.HostAllowlist
	Hub             *stream.Hub
	Models          *model.Registry
	Recordings      *recording.Manager
	Training        *training.Manager
	Calibration     *calibration.Manager
	PoseMetric      *localize.MetricCalibration
	calibrationErr  error
	poseMetricErr   error
	trainingErr     error
	EdgeRegistry    *edgeregistry.Registry
	Vendors         *vendor.Store
	Poller          *vendor.Poller
	MQTT            *mqttbridge.Publisher
	Discovery       *discovery.Advertiser
	Field           *rufield.Surface
	Ingress         *ingest.Receiver
	OAuth           *oauth.Service
	Logger          *slog.Logger
	adaptiveTrainMu sync.Mutex
	serviceMu       sync.Mutex
	serviceCancel   context.CancelFunc
	serviceWG       sync.WaitGroup
	servicesStarted bool
	closed          bool
	closeOnce       sync.Once
	fieldErr        error
	spaceMu         sync.Mutex
	spaceLayout     spaceLayout
	spaceErr        error
}

func New(cfg config.Config, store *state.Store, tickets *security.TicketStore, hosts *security.HostAllowlist, models *model.Registry, recordings *recording.Manager, logger *slog.Logger) *Server {
	if tickets == nil {
		tickets = security.NewTicketStore()
	}
	if store == nil {
		store = state.New(cfg.Source, nil)
	}
	if hosts == nil {
		hosts, _ = security.NewHostAllowlist(cfg.AllowedHosts)
	}
	if logger == nil {
		logger = slog.Default()
	}
	var edgeRegistry *edgeregistry.Registry
	if !cfg.NoEdgeRegistry && cfg.EdgeRegistryURL != "" && cfg.EdgeRegistryTTL > 0 && cfg.EdgeRegistryTTL <= config.MaxEdgeRegistryTTLSeconds {
		edgeRegistry = edgeregistry.New(cfg.EdgeRegistryURL, time.Duration(cfg.EdgeRegistryTTL)*time.Second)
	}
	fieldSurface, fieldErr := rufield.New(cfg.DataDir)
	server := &Server{Config: cfg, State: store, Tickets: tickets, Hosts: hosts, Hub: store.Hub(), Models: models, Recordings: recordings, EdgeRegistry: edgeRegistry, Vendors: vendor.NewStore(), Field: fieldSurface, fieldErr: fieldErr, Logger: logger}
	server.initSpace()
	if fieldErr != nil {
		logger.Error("RuField signing surface is unavailable", "reason", "signing_key_initialization_failed")
	}
	store.SetModelRegistry(models)
	if strings.TrimSpace(cfg.DataDir) != "" {
		server.Training, server.trainingErr = training.New(cfg.DataDir, models)
		if server.trainingErr != nil {
			logger.Error("model training endpoints are unavailable", "error", server.trainingErr)
		}
	}
	if strings.TrimSpace(cfg.DataDir) != "" {
		manager, err := calibration.New(filepath.Join(cfg.DataDir, "room_calibration.json"))
		if err != nil {
			server.calibrationErr = err
			logger.Error("room calibration state could not be loaded; calibration APIs are disabled", "error", err)
		} else {
			server.Calibration = manager
			store.SetCalibrationManager(manager)
		}
		poseSnapshot := store.PoseInference(time.Now())
		modelHash := ""
		if poseSnapshot.Enabled {
			modelHash = poseSnapshot.Model.ArtifactSHA256
		}
		poseMetric, metricErr := localize.NewMetricCalibration(filepath.Join(cfg.DataDir, "pose_metric_calibration.json"), modelHash)
		if metricErr != nil {
			server.poseMetricErr = metricErr
			logger.Error("metric pose calibration could not be loaded; metric mapping is disabled", "error", metricErr)
		} else {
			server.PoseMetric = poseMetric
		}
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	var handler http.Handler = mux
	handler = security.PrivacyMiddleware(s.Config.PrivacyMode, 16<<20, handler)
	handler = s.bearerMiddleware(handler)
	handler = noCacheMiddleware(handler)
	handler = observeRequestsMiddleware(s.Logger, handler)
	if s.Hosts != nil {
		handler = s.Hosts.Middleware(handler)
	}
	return handler
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.serviceMu.Lock()
		s.closed = true
		cancel := s.serviceCancel
		s.serviceMu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.serviceWG.Wait()
		if s.Discovery != nil {
			if err := s.Discovery.Close(); err != nil {
				s.Logger.Warn("Wave Ray local discovery shutdown failed", "reason", "advertiser_shutdown_failed")
			}
		}
		if s.Field != nil {
			s.Field.Close()
		}
		if s.Training != nil {
			s.Training.Close()
		}
	})
}

// Start launches bounded background integration consumers. It is called once
// by the server process after the process context has been created.
func (s *Server) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("HTTP API server is unavailable")
	}
	if s.Field == nil {
		if s.fieldErr != nil {
			return fmt.Errorf("RuField initialization: %w", s.fieldErr)
		}
		return errors.New("RuField surface is unavailable")
	}
	pollConfig, err := vendor.PollConfigFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("vendor polling configuration: %w", err)
	}
	poller, err := vendor.NewPoller(pollConfig, s.Vendors, os.Getenv, s.Logger)
	if err != nil {
		return fmt.Errorf("vendor poller initialization: %w", err)
	}
	mqttConfig, mqttEnabled, err := mqttbridge.ConfigFromEnv(os.Getenv, s.Config.DataDir)
	if err != nil {
		return fmt.Errorf("MQTT configuration: %w", err)
	}
	var mqttPublisher *mqttbridge.Publisher
	if mqttEnabled {
		mqttPublisher, err = mqttbridge.New(mqttConfig, s.Hub, s.Logger)
		if err != nil {
			return fmt.Errorf("MQTT publisher initialization: %w", err)
		}
		if mqttConfig.TLS == nil && !mqttbridge.IsLoopbackHost(mqttConfig.Host) {
			s.Logger.Warn("MQTT broker connection is plaintext; enable RUVIEW_MQTT_TLS for remote brokers", "host", mqttConfig.Host)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	if s.closed {
		return errors.New("HTTP API background services are already closed")
	}
	if s.servicesStarted {
		return errors.New("HTTP API background services already started")
	}
	s.servicesStarted = true
	runCtx, cancel := context.WithCancel(ctx)
	s.serviceCancel = cancel
	s.Poller = poller
	s.MQTT = mqttPublisher
	mdnsSettings := discovery.Settings{
		Disabled:         s.Config.NoMDNS,
		BindAddress:      s.Config.BindAddr,
		HTTPPort:         s.Config.HTTPPort,
		InstallationID:   s.Config.InstallationID,
		InstallationName: s.Config.InstallationName,
	}
	if discovery.ShouldAdvertise(mdnsSettings) {
		advertiser, discoveryErr := discovery.Start(runCtx, mdnsSettings)
		if discoveryErr != nil {
			s.Logger.Warn("Wave Ray local discovery unavailable; manual origin remains usable", "reason", "advertisement_unavailable")
		} else if advertiser != nil {
			s.Discovery = advertiser
			s.Logger.Info("Wave Ray local discovery advertised", "port", s.Config.HTTPPort)
		}
	}

	if s.Hub != nil {
		updates := s.Hub.Subscribe()
		s.serviceWG.Add(1)
		go func() {
			defer s.serviceWG.Done()
			defer updates.Close()
			for {
				select {
				case <-runCtx.Done():
					return
				case frame, ok := <-updates.C:
					if !ok {
						return
					}
					var update state.SensingUpdate
					if json.Unmarshal(frame, &update) != nil || update.Type != "sensing_update" {
						continue
					}
					if _, emitErr := s.Field.Emit(update); emitErr != nil {
						s.Logger.Warn("RuField event withheld", "reason", "invalid_sensing_update")
					}
				}
			}
		}()
	}
	if len(pollConfig.Sources) > 0 {
		s.serviceWG.Add(1)
		go func() {
			defer s.serviceWG.Done()
			poller.Run(runCtx)
		}()
	}
	if mqttPublisher != nil {
		s.serviceWG.Add(1)
		go func() {
			defer s.serviceWG.Done()
			mqttPublisher.Run(runCtx)
		}()
	}
	return nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.root)
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(filepath.Clean(s.Config.UIPath)))))
	mux.HandleFunc("GET /health", s.healthReady)
	mux.HandleFunc("GET /health/health", s.healthSystem)
	mux.HandleFunc("GET /health/live", s.healthLive)
	mux.HandleFunc("GET /health/ready", s.healthReady)
	mux.HandleFunc("GET /health/version", s.healthVersion)
	mux.HandleFunc("GET /health/metrics", s.healthMetrics)
	mux.HandleFunc("GET /oauth/status", s.oauthStatus)
	mux.HandleFunc("GET /oauth/start", s.oauthStart)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	mux.HandleFunc("GET /oauth/logout", s.oauthLogout)
	mux.HandleFunc("GET /api/field", s.fieldSurface)
	mux.HandleFunc("GET /ws/field", s.fieldSocket)
	mux.HandleFunc("GET /api/v1/info", s.apiInfo)
	mux.HandleFunc("GET /api/v1/status", s.healthReady)
	mux.HandleFunc("GET /api/v1/metrics", s.healthMetrics)
	mux.HandleFunc("GET /api/v1/sensing/latest", s.latest)
	mux.HandleFunc("GET /api/v1/spatial-fusion/status", s.spatialFusionStatus)
	mux.HandleFunc("GET /api/v1/nodes", s.nodes)
	mux.HandleFunc("GET /api/v1/space/layout", s.getSpace)
	mux.HandleFunc("POST /api/v1/space/layout", s.saveSpace)
	mux.HandleFunc("GET /api/v1/pose/current", s.poseCurrent)
	mux.HandleFunc("GET /api/v1/pose/status", s.poseCurrent)
	mux.HandleFunc("GET /api/v1/pose/frames", s.poseFrames)
	mux.HandleFunc("GET /api/v1/pose/zones", s.poseZones)
	mux.HandleFunc("GET /api/v1/pose/physics/metrics", s.posePhysicsMetrics)
	mux.HandleFunc("GET /api/v1/pose/stats", s.poseStats)
	mux.HandleFunc("GET /api/v1/pose/zones/summary", s.poseZonesSummary)
	mux.HandleFunc("GET /api/v1/pose/activities", s.poseActivities)
	mux.HandleFunc("POST /api/v1/pose/calibrate", s.poseCalibrate)
	mux.HandleFunc("GET /api/v1/pose/calibration/status", s.poseCalibrationStatus)
	mux.HandleFunc("GET /api/v1/introspection/snapshot", s.introspectionSnapshot)
	mux.HandleFunc("GET /api/v1/vital-signs", s.vitalSigns)
	mux.HandleFunc("GET /api/v1/edge-vitals", s.edgeVitals)
	mux.HandleFunc("GET /api/v1/trust/status", s.trustStatus)
	mux.HandleFunc("GET /api/v1/csi/mediatek/latest", s.vendorCSILatest)
	mux.HandleFunc("GET /api/v1/csi/mediatek/devices", s.mediatekDevices)
	mux.HandleFunc("GET /api/v1/csi/mediatek/devices/{device_id}/frames", s.mediatekDeviceFrames)
	mux.HandleFunc("GET /api/v1/csi/mediatek/devices/{device_id}/summary", s.mediatekDeviceSummary)
	mux.HandleFunc("GET /api/v1/csi/mediatek/activity", s.mediatekActivity)
	mux.HandleFunc("GET /api/v1/csi/qualcomm/latest", s.vendorCSILatest)
	mux.HandleFunc("GET /api/v1/csi/realtek/latest", s.vendorCSILatest)
	mux.HandleFunc("GET /api/v1/radar/latest", s.latestRadar)
	mux.HandleFunc("GET /api/v1/rf/vendors", s.vendorsUnavailable)
	mux.HandleFunc("GET /api/v1/rf/vendors/status", s.vendorPollStatus)
	mux.HandleFunc("GET /api/v1/integrations/mqtt/status", s.mqttIntegrationStatus)
	mux.HandleFunc("GET /api/v1/rf/vendors/latest", s.vendorLatestUnavailable)
	mux.HandleFunc("GET /api/v1/rf/vendors/{vendor}/latest", s.vendorLatestUnavailable)
	mux.HandleFunc("POST /api/v1/rf/vendors/{vendor}/events", s.vendorIngestUnavailable)
	mux.HandleFunc("GET /api/v1/mesh", s.mesh)
	mux.HandleFunc("GET /api/v1/mesh/metrics", s.meshMetrics)
	mux.HandleFunc("GET /api/v1/nodes/{id}/sync", s.nodeSync)
	mux.HandleFunc("GET /api/v1/edge/registry", s.edgeRegistry)
	mux.HandleFunc("GET /api/v1/wasm-events", s.wasmEvents)
	mux.HandleFunc("GET /api/v1/adaptive/status", s.adaptiveStatus)
	mux.HandleFunc("POST /api/v1/adaptive/train", s.adaptiveTrain)
	mux.HandleFunc("POST /api/v1/adaptive/unload", s.adaptiveUnload)
	mux.HandleFunc("GET /api/v1/calibration/status", s.calibrationStatus)
	mux.HandleFunc("POST /api/v1/calibration/start", s.calibrationStart)
	mux.HandleFunc("POST /api/v1/calibration/stop", s.calibrationStop)
	mux.HandleFunc("POST /api/v1/calibration/cancel", s.calibrationCancel)
	mux.HandleFunc("POST /api/v1/calibration/reset", s.calibrationReset)
	mux.HandleFunc("POST /api/v1/calibration/bootstrap/promote", s.bootstrapCapabilityUnavailable)
	mux.HandleFunc("POST /api/v1/calibration/bootstrap/refine", s.bootstrapCapabilityUnavailable)
	mux.HandleFunc("GET /api/v1/config/dedup-factor", s.dedupConfig)
	mux.HandleFunc("POST /api/v1/config/dedup-factor", s.setDedupConfig)
	mux.HandleFunc("POST /api/v1/config/ground-truth", s.setGroundTruth)
	mux.HandleFunc("GET /api/v1/stream/status", s.streamStatus)
	mux.HandleFunc("GET /api/v1/train/status", s.trainingStatus)
	mux.HandleFunc("POST /api/v1/train/start", s.trainingStart)
	mux.HandleFunc("POST /api/v1/train/stop", s.trainingStop)
	mux.HandleFunc("POST /api/v1/train/pretrain", s.trainingUnavailable)
	mux.HandleFunc("POST /api/v1/train/lora", s.trainingUnavailable)
	mux.HandleFunc("GET /api/v1/datasets", s.listDatasets)
	mux.HandleFunc("GET /api/v1/datasets/{id}", s.getDataset)
	mux.HandleFunc("GET /api/v1/models", s.listModels)
	mux.HandleFunc("GET /api/v1/models/active", s.activeModel)
	mux.HandleFunc("GET /api/v1/models/{id}", s.getModel)
	mux.HandleFunc("POST /api/v1/models/load", s.loadModel)
	mux.HandleFunc("POST /api/v1/models/unload", s.unloadModel)
	mux.HandleFunc("DELETE /api/v1/models/{id}", s.deleteModel)
	mux.HandleFunc("GET /api/v1/models/lora/profiles", s.loraProfiles)
	mux.HandleFunc("POST /api/v1/models/lora/activate", s.activateLoRA)
	mux.HandleFunc("GET /api/v1/model/info", s.modelInfo)
	mux.HandleFunc("GET /api/v1/model/layers", s.modelLayers)
	mux.HandleFunc("GET /api/v1/model/segments", s.modelSegments)
	mux.HandleFunc("GET /api/v1/model/sona/profiles", s.sonaProfiles)
	mux.HandleFunc("POST /api/v1/model/sona/activate", s.activateSONA)
	mux.HandleFunc("GET /api/v1/recording/list", s.listRecordings)
	mux.HandleFunc("POST /api/v1/recording/start", s.startRecording)
	mux.HandleFunc("POST /api/v1/recording/stop", s.stopRecording)
	mux.HandleFunc("DELETE /api/v1/recording/{id}", s.deleteRecording)
	mux.HandleFunc("POST /api/v1/ws-ticket", s.issueWSTicket)
	mux.HandleFunc("GET /ws/sensing", s.sensingSocket)
	mux.HandleFunc("GET /api/v1/stream/pose", s.poseSocket)
	mux.HandleFunc("GET /ws/introspection", s.introspectionSocket)
	mux.HandleFunc("GET /ws/train/progress", s.trainingSocket)
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if index := filepath.Join(s.Config.UIPath, "index.html"); fileExists(index) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "Wave Ray Wi-Fi sensing backend", "backend": "go", "version": serverVersion,
		"ui": "/ui/", "health": "/health/ready", "api": "/api/v1/info",
	})
}

func fileExists(path string) bool { info, err := os.Stat(path); return err == nil && !info.IsDir() }

func (s *Server) healthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive", "uptime": uint64(s.State.Uptime().Seconds())})
}

func (s *Server) healthReady(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.State.Health())
}

func (s *Server) healthSystem(w http.ResponseWriter, _ *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	health := s.State.Health()
	source, _ := health["source"].(string)
	hardwareStatus := "healthy"
	hardwareMessage := "Source: " + source
	if strings.HasSuffix(source, ":offline") {
		hardwareStatus = "degraded"
		hardwareMessage = "Source is offline: " + source
	} else if source == "simulate" {
		hardwareStatus = "simulated"
		hardwareMessage = "Synthetic simulation; no live sensor data."
	}
	signalStatus := "healthy"
	if received, ok := health["frames_received"].(uint64); ok && received == 0 {
		signalStatus = "waiting"
	}
	overallStatus := "healthy"
	if hardwareStatus != "healthy" || signalStatus == "waiting" {
		overallStatus = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": overallStatus,
		"components": map[string]any{
			"api":      map[string]any{"status": "healthy", "message": "Go net/http server"},
			"hardware": map[string]any{"status": hardwareStatus, "message": hardwareMessage},
			"signal":   map[string]any{"status": signalStatus, "message": "CSI feature pipeline"},
			"stream":   streamHealthComponent(s.State.StreamMetrics(time.Now()), s.Hub.BrowserClientCount()),
		},
		"metrics": map[string]any{"uptime_seconds": uint64(s.State.Uptime().Seconds()), "memory_alloc_bytes": mem.Alloc},
	})
}

func (s *Server) healthVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": serverVersion, "name": "Wave Ray", "backend": "go"})
}

func (s *Server) healthMetrics(w http.ResponseWriter, _ *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	metrics := map[string]any{
		"system_metrics": map[string]any{"memory": map[string]any{"alloc_bytes": mem.Alloc, "sys_bytes": mem.Sys}},
		"tick":           s.State.Health()["tick"], "websocket_clients": s.Hub.BrowserClientCount(),
	}
	if s.Ingress != nil {
		metrics["udp_ingress"] = s.Ingress.Counters()
	}
	writeJSON(w, http.StatusOK, metrics)
}

type requestIdentity struct {
	StaticToken bool
	Principal   *oauth.Principal
	Session     *oauth.BrowserSession
}

type requestIdentityKey struct{}

func identityFromRequest(r *http.Request) (requestIdentity, bool) {
	identity, ok := r.Context().Value(requestIdentityKey{}).(requestIdentity)
	return identity, ok
}

func (s *Server) authEnabled() bool {
	return s.Config.APIToken != "" || s.OAuth != nil
}

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	var session *oauth.BrowserSession
	if s.OAuth != nil {
		session = s.OAuth.Session(r.Header.Get("Cookie"))
	}
	var account, scope any
	if session != nil {
		account, scope = session.AccountID, session.Scope
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": s.authEnabled(), "oauth_enabled": s.OAuth != nil,
		"browser_signin": s.OAuth != nil && s.OAuth.Configured(), "signed_in": session != nil,
		"account": account, "scope": scope,
	})
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	if s.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_disabled", "browser sign-in is not configured on this server")
		return
	}
	location, cookie, err := s.OAuth.Begin(requestIsSecure(r, s.OAuth))
	if err != nil {
		s.Logger.Error("could not begin OAuth sign-in", "error", err)
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "browser sign-in could not be started")
		return
	}
	w.Header().Set("Location", location)
	w.Header().Add("Set-Cookie", cookie)
	w.WriteHeader(http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	secure := requestIsSecure(r, s.OAuth)
	if s.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_disabled", "browser sign-in is not configured on this server")
		return
	}
	bad := func(status int, code, message string) {
		w.Header().Add("Set-Cookie", s.OAuth.ClearTransaction(secure))
		writeError(w, status, code, message)
	}
	query := r.URL.Query()
	if values, ok := query["error"]; ok {
		if len(values) != 1 {
			bad(http.StatusBadRequest, "oauth_callback_invalid", "sign-in response is invalid")
			return
		}
		bad(http.StatusBadRequest, "oauth_denied", "the identity provider declined sign-in")
		return
	}
	code, codeOK := singleQueryValue(query, "code")
	state, stateOK := singleQueryValue(query, "state")
	if !codeOK || !stateOK {
		bad(http.StatusBadRequest, "oauth_callback_invalid", "sign-in response is incomplete")
		return
	}
	verifier, err := s.OAuth.VerifierForCallback(r.Header.Get("Cookie"), state)
	if err != nil {
		bad(http.StatusBadRequest, "oauth_transaction_invalid", "sign-in transaction is invalid or expired")
		return
	}
	token, err := s.OAuth.ExchangeCode(r.Context(), code, verifier)
	if err != nil {
		s.Logger.Warn("OAuth token exchange failed", "error", err)
		bad(http.StatusBadGateway, "oauth_exchange_failed", "the identity provider could not complete sign-in")
		return
	}
	principal, err := s.OAuth.VerifyAccessToken(r.Context(), token, "sensing:read")
	if err != nil {
		s.Logger.Warn("OAuth sign-in token rejected", "error", err)
		bad(http.StatusUnauthorized, "oauth_token_rejected", "the identity provider returned an unacceptable sign-in token")
		return
	}
	sessionCookie, err := s.OAuth.IssueSession(principal, secure)
	if err != nil {
		s.Logger.Error("could not issue browser session", "error", err)
		bad(http.StatusServiceUnavailable, "oauth_session_unavailable", "browser sign-in could not create a session")
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/ui/?signed_in=%d", time.Now().UnixMilli()))
	w.Header().Add("Set-Cookie", sessionCookie)
	w.Header().Add("Set-Cookie", s.OAuth.ClearTransaction(secure))
	w.WriteHeader(http.StatusFound)
}

func singleQueryValue(query url.Values, key string) (string, bool) {
	values, ok := query[key]
	if !ok || len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func (s *Server) oauthLogout(w http.ResponseWriter, r *http.Request) {
	secure := requestIsSecure(r, s.OAuth)
	w.Header().Set("Location", fmt.Sprintf("/ui/?signed_out=%d", time.Now().UnixMilli()))
	if s.OAuth != nil {
		w.Header().Add("Set-Cookie", s.OAuth.ClearSession(secure))
		w.Header().Add("Set-Cookie", s.OAuth.ClearTransaction(secure))
	} else {
		w.Header().Add("Set-Cookie", clearOAuthCookie("ruview_session", secure))
		w.Header().Add("Set-Cookie", clearOAuthCookie("ruview_oauth_txn", secure))
	}
	w.WriteHeader(http.StatusFound)
}

func requestIsSecure(r *http.Request, service *oauth.Service) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return service != nil && strings.HasPrefix(strings.ToLower(service.RedirectURI()), "https://")
}

func clearOAuthCookie(name string, secure bool) string {
	value := name + "=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax"
	if secure {
		value += "; Secure"
	}
	return value
}

func (s *Server) trainingStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Training == nil || s.Models == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"enabled": false, "active": false, "phase": "unavailable",
			"pose_training_enabled": false, "supported_tasks": []string{},
			"reason": "supervised activity-model training requires the Go model registry",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.Training.Status())
}

func (s *Server) trainingStart(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DatasetIDs []string `json:"dataset_ids"`
		Config     struct {
			Epochs                int     `json:"epochs"`
			BatchSize             int     `json:"batch_size"`
			LearningRate          float64 `json:"learning_rate"`
			WeightDecay           float64 `json:"weight_decay"`
			Patience              int     `json:"patience"`
			EarlyStoppingPatience int     `json:"early_stopping_patience"`
			BaseModel             string  `json:"base_model"`
			PretrainedRVF         string  `json:"pretrained_rvf"`
		} `json:"config"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if s.Training == nil {
		writeError(w, http.StatusServiceUnavailable, "training_unavailable", "supervised activity-model training is unavailable")
		return
	}
	if request.Config.BaseModel != "" || request.Config.PretrainedRVF != "" {
		writeError(w, http.StatusNotImplemented, "model_finetuning_unavailable", "fine-tuning from a base model is not implemented; select labelled recordings for a new activity classifier")
		return
	}
	patience := request.Config.Patience
	if request.Config.EarlyStoppingPatience != 0 {
		if patience != 0 && patience != request.Config.EarlyStoppingPatience {
			writeError(w, http.StatusBadRequest, "invalid_training_config", "patience and early_stopping_patience must match when both are set")
			return
		}
		patience = request.Config.EarlyStoppingPatience
	}
	status, err := s.Training.Start(request.DatasetIDs, training.Config{
		Epochs: request.Config.Epochs, BatchSize: request.Config.BatchSize,
		LearningRate: request.Config.LearningRate, WeightDecay: request.Config.WeightDecay,
		Patience: patience,
	})
	if err != nil {
		statusCode, code := http.StatusUnprocessableEntity, "training_start_rejected"
		if errors.Is(err, training.ErrTrainingActive) {
			statusCode, code = http.StatusConflict, "training_already_active"
		} else if errors.Is(err, training.ErrNoModelStore) {
			statusCode, code = http.StatusServiceUnavailable, "model_registry_unavailable"
		}
		writeError(w, statusCode, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "started", "task": "activity_classifier", "job": status,
		"detail": "Training uses labelled recordings and a per-class recording-file holdout; it does not train a pose or localization model.",
	})
}

func (s *Server) trainingStop(w http.ResponseWriter, _ *http.Request) {
	if s.Training == nil {
		writeError(w, http.StatusServiceUnavailable, "training_unavailable", "training manager is unavailable")
		return
	}
	status, err := s.Training.Stop()
	if err != nil {
		writeError(w, http.StatusConflict, "no_training_active", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelling", "job": status})
}

func (s *Server) trainingUnavailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"status": "unavailable", "enabled": false, "capability": r.URL.Path,
		"reason": "Go supports supervised activity-classifier training only; contrastive pretraining, pose-model training, and LoRA fine-tuning are not implemented.",
		"detail": "No training job was started.",
	})
}

func (s *Server) listDatasets(w http.ResponseWriter, _ *http.Request) {
	if s.Training == nil {
		writeError(w, http.StatusServiceUnavailable, "datasets_unavailable", "training dataset registry is unavailable")
		return
	}
	datasets, err := s.Training.Datasets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dataset_scan_failed", "could not scan training recordings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"datasets": datasets, "count": len(datasets), "label_format": "train_<class>_<recording>.jsonl",
		"validation_split": "recording_file_holdout",
	})
}

func (s *Server) getDataset(w http.ResponseWriter, r *http.Request) {
	if s.Training == nil {
		writeError(w, http.StatusServiceUnavailable, "datasets_unavailable", "training dataset registry is unavailable")
		return
	}
	dataset, err := s.Training.Dataset(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "dataset_not_found", "training dataset was not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "dataset_inspection_failed", "could not inspect training dataset")
		return
	}
	writeJSON(w, http.StatusOK, dataset)
}

func (s *Server) apiInfo(w http.ResponseWriter, _ *http.Request) {
	health := s.State.Health()
	_, adaptiveModelLoaded := s.State.AdaptiveModel()
	pose := s.State.PoseInference(time.Now())
	trainingEnabled := s.Training != nil && s.Models != nil
	writeJSON(w, http.StatusOK, map[string]any{
		"version": serverVersion, "backend": "go",
		"source":            health["source"],
		"measurement_modes": map[string]string{"wifi": "rssi_derived; not CSI"},
		"features": map[string]any{
			"wifi_sensing": true, "signal_processing": true, "streaming": true,
			"room_classification_fusion":  true,
			"pose_estimation":             pose.Enabled,
			"experimental_pose_inference": pose.Enabled,
			"person_localization":         false, "multi_person_pose": false,
			"pose_model_quality_validated": pose.Enabled && pose.Model.QualityValidated,
			"calibrated_vitals":            false, "server_training": trainingEnabled,
			"activity_model_training": trainingEnabled, "pose_model_training": false,
			"adaptive_activity_training": true, "adaptive_activity_model_loaded": adaptiveModelLoaded,
			"host_wifi_capture":   runtime.GOOS == "windows",
			"vendor_rf_adapters":  true,
			"vendor_csi_adapters": true,
			"mesh_sync":           true, "mesh_time_fusion": true, "spatial_csi_fusion": true,
			"oauth_browser_signin": s.OAuth != nil && s.OAuth.Configured(),
		},
	})
}

func (s *Server) spatialFusionStatus(w http.ResponseWriter, _ *http.Request) {
	update, status, age := s.State.Latest(time.Now())
	if status == "no data yet" {
		writeJSON(w, http.StatusOK, map[string]any{
			"stream_status": status,
			"fusion": map[string]any{
				"status": "unavailable", "reason": "no_fresh_raw_csi_frames",
				"available": false, "fused": false, "time_alignment": "unavailable",
				"active_nodes": 0, "required_nodes": 2,
				"spatial_localization_available": false,
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stream_status": status, "age_ms": age.Milliseconds(), "fusion": update.SpatialFusion,
	})
}

func (s *Server) latest(w http.ResponseWriter, _ *http.Request) {
	update, status, age := s.State.Latest(time.Now())
	if status == "no data yet" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no data yet"})
		return
	}
	if status == "stale" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "stale", "age_ms": age.Milliseconds(), "source": update.Source})
		return
	}
	result, err := json.Marshal(update)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "serialization_failed", "could not serialize sensing snapshot")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		writeError(w, http.StatusInternalServerError, "serialization_failed", "could not serialize sensing snapshot")
		return
	}
	payload["age_ms"] = age.Milliseconds()
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) nodes(w http.ResponseWriter, _ *http.Request) {
	nodes := s.State.NodeStatuses(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "total": len(nodes)})
}

func (s *Server) poseCurrent(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view == "refined" || view == "both" {
		writeError(w, http.StatusConflict, "pose_physics_unavailable", "no calibrated metric pose is available from the current sensing data")
		return
	}
	if view != "" && view != "raw" {
		writeError(w, http.StatusBadRequest, "invalid_pose_view", "view must be raw, refined, or both")
		return
	}
	update, status, age := s.State.Latest(time.Now())
	now := time.Now()
	poseStatus := s.State.PoseInference(now)
	result := map[string]any{
		"timestamp": time.Now().UnixMilli() / 1000.0, "persons": []any{}, "source": update.Source,
		"pose_available": false, "image_pose_available": false,
		"person_detection_available": false, "multi_person_pose_available": false,
		"localization_available": false, "metric_candidate_available": false,
		"location_status": "unavailable", "location_reason": "no validated multi-person metric localization model is active",
		"status": status, "signal_hotspots": fieldHotspots(update), "hotspots_are_people": false,
		"measurement_authority": "unverified", "pose_inference": poseStatus,
	}
	if poseStatus.Estimate != nil {
		estimate := poseStatus.Estimate
		result["image_pose_available"] = true
		result["experimental_pose"] = map[string]any{
			"model": poseStatus.Model.ID, "model_sha256": poseStatus.Model.ArtifactSHA256,
			"coordinate_space": "normalized_image", "keypoints": estimate.Prediction.Keypoints,
			"node_id": estimate.NodeID, "timestamp": estimate.Timestamp,
			"evidence": estimate.Evidence, "per_frame_confidence": nil,
			"is_person_detection": false,
		}
		result["location_status"] = "experimental_image_pose_only"
		result["location_reason"] = "pose_v1 is single-person regression without a detector, confidence head, or accepted quality baseline"
		if s.PoseMetric != nil && poseStatus.Status == "experimental" {
			ankles := estimate.Prediction.FloorPoint()
			if room, mapErr := s.PoseMetric.Map([2]float64{float64(ankles.X), float64(ankles.Y)}); mapErr == nil {
				metricStatus := s.PoseMetric.Status()
				result["metric_candidate_available"] = true
				result["experimental_metric_candidate"] = map[string]any{
					"x": room[0], "y": room[1], "coordinate_frame": "room_floor_m",
					"method": "homography_of_regressed_ankle_midpoint", "authority": "unverified",
					"calibration_leave_one_out_rmse_m": metricStatus.LeaveOneOutRMSE,
					"is_person_detection":              false, "person_localization_validated": false,
					"reason": "calibration validates only the coordinate transform; pose model quality is not accepted",
				}
			}
		}
	}
	if update.EstimatedPersons != nil {
		result["total_persons"] = *update.EstimatedPersons
		result["count_source"] = "edge_reported_count_unverified"
	} else {
		result["total_persons"] = nil
		result["count_source"] = "unavailable"
	}
	if status == "fresh" {
		result["classification"] = update.Classification
		result["age_ms"] = age.Milliseconds()
	} else if status == "stale" {
		result["age_ms"] = age.Milliseconds()
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) poseFrames(w http.ResponseWriter, _ *http.Request) {
	poseStatus := s.State.PoseInference(time.Now())
	frames := []any{}
	if poseStatus.Estimate != nil {
		estimate := poseStatus.Estimate
		frames = append(frames, map[string]any{
			"timestamp": estimate.Timestamp, "node_id": estimate.NodeID, "source": estimate.Source,
			"evidence": estimate.Evidence, "coordinate_space": "normalized_image",
			"keypoints": estimate.Prediction.Keypoints, "is_person_detection": false,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"frames": frames, "count": len(frames), "available": len(frames) > 0,
		"frames_processed": poseStatus.FramesProcessed, "person_detection_available": false,
		"message": poseStatus.Reason,
	})
}

func (s *Server) poseZones(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"zones": []any{}, "count": 0, "configured": false,
		"message": "No calibrated spatial zones are configured.",
	})
}

func fieldHotspots(update state.SensingUpdate) []localize.Peak {
	if !update.Classification.Presence {
		return []localize.Peak{}
	}
	nx, nz := update.SignalField.GridSize[0], update.SignalField.GridSize[2]
	return localize.ExtractPeaks(update.SignalField.Values, nx, nz, 8, 3)
}

func (s *Server) posePhysicsMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, "# HELP ruview_pose_physics_available Whether calibrated pose refinement is active.\n# TYPE ruview_pose_physics_available gauge\nruview_pose_physics_available 0\n")
}

func (s *Server) poseStats(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	_, status, _ := s.State.Latest(now)
	health := s.State.Health()
	pose := s.State.PoseInference(now)
	var imagePoseAvailable bool
	if pose.Estimate != nil && pose.Status == "experimental" {
		imagePoseAvailable = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_detections": 0, "average_confidence": nil,
		"frames_processed": health["frames_received"], "source": health["source"],
		"pose_frames_processed": pose.FramesProcessed, "image_pose_available": imagePoseAvailable,
		"pose_available": false, "person_detection_available": false, "localization_available": false,
		"pose_status": pose.Status, "pose_reason": pose.Reason, "sensing_status": status,
	})
}

func (s *Server) poseZonesSummary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"zones": map[string]any{}, "configured": false,
		"message": "No calibrated zones are configured.",
	})
}

func (s *Server) poseActivities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"activities": []any{}, "total": 0, "persisted": false,
		"message": "Activity history is not persisted by the Go sensing server.",
	})
}

func (s *Server) poseCalibrate(w http.ResponseWriter, r *http.Request) {
	if s.PoseMetric == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error_code": "metric_calibration_unavailable",
			"error": "pose model or calibration storage is not available",
		})
		return
	}
	var request struct {
		Action string   `json:"action"`
		RoomX  *float64 `json:"room_x_m,omitempty"`
		RoomY  *float64 `json:"room_y_m,omitempty"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	switch request.Action {
	case "capture":
		if request.RoomX == nil || request.RoomY == nil {
			writeError(w, http.StatusBadRequest, "ground_truth_required", "capture requires room_x_m and room_y_m in meters")
			return
		}
		poseStatus := s.State.PoseInference(time.Now())
		if poseStatus.Estimate == nil || poseStatus.Status != "experimental" || time.Since(poseStatus.Estimate.Timestamp) > 2*time.Second {
			writeError(w, http.StatusConflict, "pose_sample_unavailable", "a fresh experimental pose estimate is required to capture an anchor")
			return
		}
		if poseStatus.Estimate.Source != "esp32" || poseStatus.Estimate.Evidence != "unverified" {
			writeError(w, http.StatusConflict, "hardware_evidence_required", "metric calibration anchors must come from live ESP32 CSI; simulation is not accepted")
			return
		}
		imagePoint := poseStatus.Estimate.Prediction.FloorPoint()
		if err := s.PoseMetric.AddAnchor(localize.MetricAnchor{
			Image: [2]float64{float64(imagePoint.X), float64(imagePoint.Y)},
			Room:  [2]float64{*request.RoomX, *request.RoomY},
		}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error_code": "invalid_anchor", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"success": true, "captured": true, "anchor_count": s.PoseMetric.Status().AnchorCount,
			"image_coordinate_space": "normalized_image", "room_coordinate_frame": "room_floor_m",
			"evidence": poseStatus.Estimate.Evidence,
		})
	case "fit":
		if err := s.PoseMetric.Fit(); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error_code": "metric_calibration_fit_failed", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "calibration": s.PoseMetric.Status()})
	case "reset":
		if err := s.PoseMetric.Reset(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error_code": "metric_calibration_reset_failed", "error": "metric calibration could not be removed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "calibration": s.PoseMetric.Status()})
	default:
		writeError(w, http.StatusBadRequest, "invalid_calibration_action", "action must be capture, fit, or reset")
	}
}

func (s *Server) introspectionSnapshot(w http.ResponseWriter, _ *http.Request) {
	update, _, _ := s.State.Latest(time.Now())
	writeJSON(w, http.StatusOK, introspectionSnapshot(update))
}

func (s *Server) wasmEvents(w http.ResponseWriter, _ *http.Request) {
	if packet, ok := s.State.LatestWASMOutput(); ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "wasm_events": packet})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "no_data", "wasm_events": nil,
		"message": "No WASM output packet received yet. Upload and start a .wasm module on the ESP32.",
	})
}

func (s *Server) noVendorData(w http.ResponseWriter, r *http.Request) {
	message := "no vendor CSI data yet"
	switch r.URL.Path {
	case "/api/v1/csi/mediatek/latest":
		message = "no MediaTek CSI data yet"
	case "/api/v1/csi/qualcomm/latest":
		message = "no Qualcomm CSI data yet"
	case "/api/v1/csi/realtek/latest":
		message = "no Realtek CSI data yet"
	case "/api/v1/radar/latest":
		message = "no radar data yet"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": message, "source_available": false})
}

func (s *Server) vendorCSILatest(w http.ResponseWriter, r *http.Request) {
	eventType, emptyMessage := "", ""
	switch r.URL.Path {
	case "/api/v1/csi/mediatek/latest":
		eventType, emptyMessage = "mediatek_csi", "no MediaTek CSI data yet"
	case "/api/v1/csi/qualcomm/latest":
		eventType, emptyMessage = "qualcomm_csi", "no Qualcomm CSI data yet"
	case "/api/v1/csi/realtek/latest":
		eventType, emptyMessage = "realtek_csi", "no Realtek RTL8721Dx CSI data yet"
	default:
		writeError(w, http.StatusNotFound, "unknown_csi_vendor", "vendor CSI route is not registered")
		return
	}
	if snapshot, ok := s.State.LatestVendorCSI(eventType); ok {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": emptyMessage})
}

func (s *Server) latestRadar(w http.ResponseWriter, _ *http.Request) {
	snapshot, ok := s.State.LatestRadar()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no Realtek radar data yet"})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) mediatekDevices(w http.ResponseWriter, _ *http.Request) {
	devices := s.State.MediaTekCSIDevices(time.Now())
	results := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		encoded, err := json.Marshal(device.Snapshot)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "serialization_failed", "could not serialize MediaTek device snapshot")
			return
		}
		var result map[string]any
		if err := json.Unmarshal(encoded, &result); err != nil {
			writeError(w, http.StatusInternalServerError, "serialization_failed", "could not serialize MediaTek device snapshot")
			return
		}
		result["age_ms"] = device.AgeMS
		results = append(results, result)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(results), "devices": results})
}

func (s *Server) mediatekDeviceSummary(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	summary, ok := s.State.MediaTekCSISummary(deviceID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown device_id", "device_id": deviceID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":        deviceID,
		"subcarrier_count": summary.SubcarrierCount,
		"ring_frame_count": summary.FrameCount,
		"ring_span_ms":     summary.SpanMS,
		"frame_rate_hz":    summary.FrameRateHz,
		"rssi_by_rx_chain": summary.RSSIByRX,
		"chains":           summary.Chains,
		"heuristic":        nil,
	})
}

func (s *Server) mediatekDeviceFrames(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	n := 64
	if raw := r.URL.Query().Get("n"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_frame_count", "n must be a non-negative integer")
			return
		}
		n = min(parsed, csivendor.MaxRingFrames)
	}
	frames, ok := s.State.MediaTekCSIFrames(deviceID, n)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown device_id", "device_id": deviceID})
		return
	}
	wantAmp, wantPhase := true, true
	if fields, present := r.URL.Query()["fields"]; present {
		if len(fields) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_fields", "fields may be specified only once")
			return
		}
		wantAmp, wantPhase = false, false
		for _, field := range strings.Split(fields[0], ",") {
			switch strings.TrimSpace(field) {
			case "amp":
				wantAmp = true
			case "phase":
				wantPhase = true
			}
		}
	}
	chainPositions := make([]map[string]any, 0, len(frames.Chains))
	for _, chain := range frames.Chains {
		chainPositions = append(chainPositions, map[string]any{"tx": chain.TX, "rx": chain.RX})
	}
	frameResults := make([]map[string]any, 0, len(frames.Frames))
	for _, frame := range frames.Frames {
		frameChains := make([]map[string]any, 0, len(frame.Chains))
		amplitudes := make([][]float32, 0, len(frame.Chains))
		phases := make([][]float32, 0, len(frame.Chains))
		for _, chain := range frame.Chains {
			frameChains = append(frameChains, map[string]any{"tx": chain.TX, "rx": chain.RX})
			amplitudes = append(amplitudes, chain.Amplitude)
			phases = append(phases, chain.Phase)
		}
		row := map[string]any{
			"ts_us": frame.TimestampUS, "seq": frame.Sequence,
			"rssi": frame.RSSIDBm, "chains": frameChains,
		}
		if wantAmp {
			row["amp"] = amplitudes
		}
		if wantPhase {
			row["phase"] = phases
		}
		frameResults = append(frameResults, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID, "subcarrier_count": frames.SubcarrierCount,
		"chains": chainPositions, "frames": frameResults,
	})
}

func (s *Server) mediatekActivity(w http.ResponseWriter, r *http.Request) {
	seconds := uint64(300)
	if raw := r.URL.Query().Get("seconds"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_seconds", "seconds must be a non-negative integer")
			return
		}
		seconds = min(parsed, uint64(300))
	}
	samples := s.State.MediaTekActivityHistory(time.Now(), seconds)
	writeJSON(w, http.StatusOK, map[string]any{
		"seconds": seconds, "sample_count": len(samples), "samples": samples,
	})
}

func (s *Server) vendorsUnavailable(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, vendor.Descriptors())
}

func (s *Server) vendorPollStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Poller == nil {
		writeJSON(w, http.StatusOK, []vendor.PollStatus{})
		return
	}
	writeJSON(w, http.StatusOK, s.Poller.Status())
}

func (s *Server) mqttIntegrationStatus(w http.ResponseWriter, _ *http.Request) {
	if s.MQTT == nil {
		writeJSON(w, http.StatusOK, mqttbridge.Status{Enabled: false, State: "disabled"})
		return
	}
	writeJSON(w, http.StatusOK, s.MQTT.Status())
}

func (s *Server) vendorLatestUnavailable(w http.ResponseWriter, r *http.Request) {
	if vendorName := r.PathValue("vendor"); vendorName != "" {
		id := vendor.ID(vendorName)
		if _, ok := vendor.DescriptorFor(id); !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown vendor"})
			return
		}
		if snapshot, ok := s.Vendors.Latest(id); ok {
			writeJSON(w, http.StatusOK, snapshot)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "no vendor RF data yet", "vendor": id})
		return
	}
	writeJSON(w, http.StatusOK, s.Vendors.All())
}

func (s *Server) vendorIngestUnavailable(w http.ResponseWriter, r *http.Request) {
	name := vendor.ID(r.PathValue("vendor"))
	if _, ok := vendor.DescriptorFor(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown vendor", "vendor": name})
		return
	}
	body := http.MaxBytesReader(w, r.Body, vendor.MaxPayload)
	payload, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "payload_too_large", "vendor": name})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_payload", "vendor": name})
		}
		return
	}
	events, err := vendor.Decode(name, payload)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, vendor.ErrUnsupported), errors.Is(err, vendor.ErrContractRequired), errors.Is(err, vendor.ErrCredentialsRequired):
			status = http.StatusForbidden
		}
		if errors.Is(err, vendor.ErrUnsupported) {
			status = http.StatusNotImplemented
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "vendor": name})
		return
	}
	snapshots := make([]vendor.Snapshot, 0, len(events))
	for _, event := range events {
		snapshot, snapshotErr := vendor.NewSnapshot(event)
		if snapshotErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": snapshotErr.Error(), "vendor": name})
			return
		}
		snapshots = append(snapshots, snapshot)
	}
	for _, snapshot := range snapshots {
		if err := s.Vendors.Put(snapshot.Event); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "vendor": name})
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"vendor": name, "accepted": len(snapshots)})
}

func (s *Server) mesh(w http.ResponseWriter, _ *http.Request) {
	nodes := s.State.MeshSync(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "total": len(nodes)})
}

func (s *Server) meshMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	nodes := s.State.MeshSync(time.Now())
	total := s.State.NodeCount()
	leaders, followers := 0, 0
	for _, node := range nodes {
		if node.IsLeader {
			leaders++
		} else {
			followers++
		}
	}
	_, _ = fmt.Fprintf(w, "# HELP wifi_densepose_mesh_node_total Per-state node count across the fleet\n# TYPE wifi_densepose_mesh_node_total gauge\nwifi_densepose_mesh_node_total{state=\"leader\"} %d\nwifi_densepose_mesh_node_total{state=\"follower\"} %d\nwifi_densepose_mesh_node_total{state=\"no_sync\"} %d\n", leaders, followers, max(0, total-len(nodes)))
	metrics := []struct {
		name  string
		help  string
		value func(state.SyncSnapshot) string
	}{
		{"wifi_densepose_mesh_offset_us", "Cross-board mesh-aligned offset, microseconds (signed)", func(v state.SyncSnapshot) string { return strconv.FormatInt(v.OffsetUS, 10) }},
		{"wifi_densepose_mesh_is_leader", "1 if this node is the elected mesh leader, else 0", func(v state.SyncSnapshot) string { return boolMetric(v.IsLeader) }},
		{"wifi_densepose_mesh_is_valid", "1 if this node has heard a fresh leader beacon, else 0", func(v state.SyncSnapshot) string { return boolMetric(v.IsValid) }},
		{"wifi_densepose_mesh_smoothed", "1 once the firmware-side EMA filter has seeded, else 0", func(v state.SyncSnapshot) string { return boolMetric(v.Smoothed) }},
		{"wifi_densepose_mesh_sequence", "High-water CSI sequence at sync emit time", func(v state.SyncSnapshot) string { return strconv.FormatUint(uint64(v.Sequence), 10) }},
		{"wifi_densepose_mesh_csi_fps", "Per-node measured CSI frame rate (Hz)", func(v state.SyncSnapshot) string { return strconv.FormatFloat(v.CSIFPSEMA, 'f', 3, 64) }},
		{"wifi_densepose_mesh_csi_fps_samples", "How many inter-frame deltas the fps EMA has seen", func(v state.SyncSnapshot) string { return strconv.FormatUint(uint64(v.CSIFPSSamples), 10) }},
		{"wifi_densepose_mesh_staleness_ms", "Milliseconds since the host last received this node's sync packet", func(v state.SyncSnapshot) string {
			if v.StalenessMS == nil {
				return "0"
			}
			return strconv.FormatUint(*v.StalenessMS, 10)
		}},
	}
	ids := make([]int, 0, len(nodes))
	for id := range nodes {
		parsed, err := strconv.Atoi(id)
		if err == nil {
			ids = append(ids, parsed)
		}
	}
	sort.Ints(ids)
	for _, metric := range metrics {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", metric.name, metric.help, metric.name)
		for _, id := range ids {
			node := nodes[strconv.Itoa(id)]
			_, _ = fmt.Fprintf(w, "%s{node=\"%d\"} %s\n", metric.name, id, metric.value(node))
		}
	}
}

func (s *Server) nodeSync(w http.ResponseWriter, r *http.Request) {
	parsedID, err := strconv.ParseUint(r.PathValue("id"), 10, 8)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_node_id", "node id must be an unsigned 8-bit integer")
		return
	}
	id := uint8(parsedID)
	snapshot, status := s.State.NodeSync(id, time.Now())
	switch status {
	case "unknown_node":
		writeJSON(w, http.StatusNotFound, map[string]any{"error": status, "node_id": id})
	case "no_sync":
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": status, "node_id": id,
			"hint": "node has not emitted a sync packet yet (no mesh peer or firmware before v0.6.9)",
		})
	default:
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func boolMetric(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (s *Server) edgeRegistry(w http.ResponseWriter, r *http.Request) {
	if s.EdgeRegistry == nil {
		writeError(w, http.StatusNotFound, "edge_registry_disabled", "the edge registry is disabled")
		return
	}
	forceRefresh := edgeregistry.ForceRefreshRequested(r.URL.Query().Get("refresh"))
	if forceRefresh {
		s.Logger.Debug("edge registry forced refresh requested")
	}
	response, err := s.EdgeRegistry.Get(r.Context(), forceRefresh)
	if err != nil {
		s.Logger.Warn("edge registry fetch failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "edge_registry_unavailable", "the edge registry is unavailable and no cached copy is available")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) adaptiveStatus(w http.ResponseWriter, _ *http.Request) {
	model, loaded := s.State.AdaptiveModel()
	if !loaded {
		writeJSON(w, http.StatusOK, map[string]any{
			"loaded": false, "message": "No adaptive model. POST /api/v1/adaptive/train to train one.",
		})
		return
	}
	writeJSON(w, http.StatusOK, adaptiveStatusResponse(model))
}

func (s *Server) adaptiveTrain(w http.ResponseWriter, r *http.Request) {
	if !s.adaptiveTrainMu.TryLock() {
		writeError(w, http.StatusConflict, "adaptive_training_active", "another adaptive training request is already running")
		return
	}
	defer s.adaptiveTrainMu.Unlock()
	model, err := adaptive.Train(r.Context(), filepath.Join(s.Config.DataDir, "recordings"))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusRequestTimeout, "training_cancelled", "adaptive training was cancelled")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := s.State.SetAdaptiveModel(model); err != nil {
		writeError(w, http.StatusInternalServerError, "adaptive_model_invalid", "trained model could not be activated")
		return
	}
	persisted := true
	if err := model.Save(filepath.Join(s.Config.DataDir, "adaptive_model.json")); err != nil {
		persisted = false
		s.Logger.Error("trained adaptive model is active but could not be persisted", "error", err)
	}
	classStats := make([]map[string]any, 0, len(model.ClassStats))
	for _, class := range model.ClassStats {
		classStats = append(classStats, map[string]any{
			"class": class.Label, "samples": class.Count, "feature_means": class.Mean,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "trained_frames": model.TrainedFrames,
		"accuracy": model.TrainingAccuracy, "accuracy_kind": "in_sample",
		"persisted": persisted, "class_stats": classStats,
	})
}

func (s *Server) adaptiveUnload(w http.ResponseWriter, _ *http.Request) {
	if err := s.State.SetAdaptiveModel(nil); err != nil {
		writeError(w, http.StatusInternalServerError, "adaptive_model_unload_failed", "adaptive model could not be unloaded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Adaptive model unloaded."})
}

func adaptiveStatusResponse(model *adaptive.Model) map[string]any {
	return map[string]any{
		"loaded": true, "trained_frames": model.TrainedFrames,
		"accuracy": model.TrainingAccuracy, "accuracy_kind": "in_sample",
		"version": model.Version, "classes": model.ClassNames,
		"class_stats": model.ClassStats,
	}
}

func (s *Server) bootstrapCapabilityUnavailable(w http.ResponseWriter, r *http.Request) {
	code, detail := "bootstrap_not_active", "A restored startup baseline must be active before refinement."
	if r.URL.Path == "/api/v1/calibration/bootstrap/refine" {
		query := r.URL.Query()
		confirmedValues := query["confirmed_empty"]
		if len(confirmedValues) > 1 {
			writeError(w, http.StatusBadRequest, "invalid_confirmed_empty", "confirmed_empty must be specified once")
			return
		}
		confirmedValue := ""
		if len(confirmedValues) == 1 {
			confirmedValue = confirmedValues[0]
		}
		if confirmedValue != "" && confirmedValue != "true" && confirmedValue != "false" {
			writeError(w, http.StatusBadRequest, "invalid_confirmed_empty", "confirmed_empty must be a boolean")
			return
		}
		if confirmedValue != "true" {
			code, detail = "empty_confirmation_required", "Explicit empty-room confirmation is required for bootstrap refinement."
		}
		sourceValues := query["source_node_id"]
		if len(sourceValues) > 1 {
			writeError(w, http.StatusBadRequest, "invalid_source_node_id", "source_node_id must be specified once")
			return
		}
		if len(sourceValues) == 1 && sourceValues[0] != "" {
			sourceNodeID := sourceValues[0]
			if _, err := strconv.ParseUint(sourceNodeID, 10, 8); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_source_node_id", "source_node_id must be an unsigned 8-bit integer")
				return
			}
		}
	} else {
		var request struct {
			BootEpoch     *string  `json:"boot_epoch"`
			SessionID     *string  `json:"session_id"`
			BindingDigest *string  `json:"binding_digest"`
			SourceNodeIDs *[]uint8 `json:"source_node_ids"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_calibration_identity", "request body must contain a valid calibration identity")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_calibration_identity", "request body must contain exactly one calibration identity")
			return
		}
		if request.BootEpoch == nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_calibration_identity", "boot_epoch is required")
			return
		}
		identity := calibration.Identity{
			BootEpoch: *request.BootEpoch, SessionID: request.SessionID,
			BindingDigest: request.BindingDigest,
		}
		if request.SourceNodeIDs != nil {
			identity.SourceNodeIDs = make([]int, len(*request.SourceNodeIDs))
			for i, nodeID := range *request.SourceNodeIDs {
				identity.SourceNodeIDs[i] = int(nodeID)
			}
		}
		if s.Calibration == nil {
			writeCalibrationUnavailable(w, s.calibrationErr)
			return
		}
		if !s.Calibration.MatchesIdentity(identity) {
			code, detail = "calibration_identity_mismatch", "The calibration request does not match the active boot, session, room binding, and node set."
		} else if s.Calibration.Status(time.Now())["calibrated"] != true {
			code, detail = "calibration_not_finalized", "Finalize an explicit room calibration before validating a startup baseline."
		} else {
			writeError(w, http.StatusNotImplemented, "bootstrap_unavailable", "startup-baseline promotion is not supported by the Go calibration manager")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false, "error_code": code, "error": detail,
	})
}

func (s *Server) streamStatus(w http.ResponseWriter, _ *http.Request) {
	status := s.State.Health()
	metrics := s.State.StreamMetrics(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"active": metrics.Active, "clients": s.Hub.BrowserClientCount(),
		"fps": metrics.FPS, "csi_fps_total": metrics.CSIFPSTotal,
		"nodes": metrics.Nodes, "source": status["source"],
	})
}

func streamHealthComponent(metrics state.StreamMetrics, clients int) map[string]any {
	status := "idle"
	if clients > 0 && metrics.FPS > 0 {
		status = "healthy"
	}
	return map[string]any{
		"status": status, "message": fmt.Sprintf("%d client(s), %.1f updates/s", clients, metrics.FPS),
		"clients": clients, "fps": metrics.FPS,
	}
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	models, err := s.Models.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_scan_failed", "could not scan model directory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models)})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	info, err := s.Models.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "model_not_found", "model was not found")
			return
		}
		if errors.Is(err, model.ErrInvalidID) {
			writeError(w, http.StatusBadRequest, "invalid_model_id", "model id is invalid")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "model_invalid", "model file could not be parsed")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) loadModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelID string `json:"model_id"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	active, err := s.Models.Load(request.ModelID)
	if err != nil {
		status := http.StatusUnprocessableEntity
		code := "model_load_failed"
		if errors.Is(err, os.ErrNotExist) {
			status, code = http.StatusNotFound, "model_not_found"
		} else if errors.Is(err, model.ErrInvalidID) {
			status, code = http.StatusBadRequest, "invalid_model_id"
		}
		writeError(w, status, code, "model could not be loaded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "loaded", "model_id": active.ModelID, "weight_count": active.WeightCount})
}

func (s *Server) unloadModel(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	id, err := s.Models.Unload()
	if err != nil {
		writeError(w, http.StatusConflict, "no_model_loaded", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unloaded", "model_id": id})
}

func (s *Server) activeModel(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	active, ok := s.Models.Active()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no_model", "message": "No model is currently loaded."})
		return
	}
	writeJSON(w, http.StatusOK, active)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.Models.Delete(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "model_not_found", "model was not found")
			return
		}
		if errors.Is(err, model.ErrInvalidID) {
			writeError(w, http.StatusBadRequest, "invalid_model_id", "model id is invalid")
			return
		}
		writeError(w, http.StatusConflict, "model_delete_rejected", "model could not be deleted in its current state")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "deleted": id})
}

func (s *Server) loraProfiles(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	id, profiles, active := s.Models.LoRAProfiles()
	writeJSON(w, http.StatusOK, map[string]any{"model_id": id, "profiles": profiles, "active": active})
}

func (s *Server) activateLoRA(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelID     string `json:"model_id"`
		ProfileName string `json:"profile_name"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	if err := s.Models.ActivateLoRA(request.ModelID, request.ProfileName); err != nil {
		writeError(w, http.StatusConflict, "lora_activation_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "activated", "model_id": request.ModelID, "profile_name": request.ProfileName})
}

func (s *Server) modelInfo(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	info, ok := s.Models.ActiveContainerInfo()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no_model", "model_loaded": false})
		return
	}
	infoMap := map[string]any{"model_loaded": true, "inference_available": false, "container": info}
	if active, activeOK := s.Models.Active(); activeOK {
		infoMap["model_id"] = active.ModelID
		infoMap["pose_source"] = active.PoseSource
	}
	writeJSON(w, http.StatusOK, infoMap)
}

func (s *Server) modelLayers(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	segments, ok := s.Models.ActiveSegments()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"layers": []any{}, "model_loaded": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"layers": segments, "model_loaded": true, "inference_available": false})
}

func (s *Server) modelSegments(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	segments, ok := s.Models.ActiveSegments()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"segments": []any{}, "count": 0})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": segments, "count": len(segments)})
}

func (s *Server) sonaProfiles(w http.ResponseWriter, _ *http.Request) {
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	id, profiles, active := s.Models.LoRAProfiles()
	writeJSON(w, http.StatusOK, map[string]any{"model_id": id, "profiles": profiles, "active": active})
}

func (s *Server) activateSONA(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelID     string `json:"model_id"`
		ProfileName string `json:"profile_name"`
		Profile     string `json:"profile"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.ProfileName == "" {
		request.ProfileName = request.Profile
	}
	if s.Models == nil {
		writeError(w, http.StatusServiceUnavailable, "model_registry_unavailable", "model registry is not configured")
		return
	}
	if err := s.Models.ActivateLoRA(request.ModelID, request.ProfileName); err != nil {
		writeError(w, http.StatusConflict, "profile_activation_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "activated", "model_id": request.ModelID, "profile_name": request.ProfileName})
}

func (s *Server) listRecordings(w http.ResponseWriter, _ *http.Request) {
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "recording_unavailable", "recording manager is not configured")
		return
	}
	items, err := s.Recordings.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recording_scan_failed", "could not scan recordings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recordings": items})
}

func (s *Server) startRecording(w http.ResponseWriter, r *http.Request) {
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "recording_unavailable", "recording manager is not configured")
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSONBody(w, r, &body) {
		return
	}
	var id string
	if raw, ok := body["id"]; ok {
		if err := json.Unmarshal(raw, &id); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_recording_id", "recording id must be a string")
			return
		}
	}
	recordingID, watermark, err := s.Recordings.Start(id, s.Config.Source)
	if err != nil {
		switch {
		case errors.Is(err, recording.ErrInvalidID):
			writeError(w, http.StatusBadRequest, "invalid_recording_id", "recording id is invalid")
		case errors.Is(err, recording.ErrRecordingActive):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "recording already in progress", "recording_id": recordingID})
		default:
			writeError(w, http.StatusConflict, "recording_start_failed", "could not start recording")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "recording_id": recordingID, "watermark": watermark})
}

func (s *Server) stopRecording(w http.ResponseWriter, _ *http.Request) {
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "recording_unavailable", "recording manager is not configured")
		return
	}
	id, frames, duration, err := s.Recordings.Stop()
	if errors.Is(err, recording.ErrNoActiveRecording) {
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "no recording in progress"})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recording_write_failed", "recording stopped because data could not be written")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "recording_id": id, "duration_secs": int64(duration.Seconds()), "frames": frames,
	})
}

func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request) {
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "recording_unavailable", "recording manager is not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.Recordings.Delete(id); err != nil {
		switch {
		case errors.Is(err, recording.ErrInvalidID):
			writeError(w, http.StatusBadRequest, "invalid_recording_id", "recording id is invalid")
		case errors.Is(err, recording.ErrRecordingNotFound):
			writeError(w, http.StatusNotFound, "recording_not_found", "recording was not found")
		case errors.Is(err, recording.ErrActiveDelete):
			writeError(w, http.StatusConflict, "recording_active", "active recording cannot be deleted")
		default:
			writeError(w, http.StatusInternalServerError, "recording_delete_failed", "recording could not be deleted")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": id})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is missing or invalid")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) issueWSTicket(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"ticket": nil, "expires_in_secs": 0,
			"usage": "append as ?ticket=<value> to the WebSocket URL; valid once",
		})
		return
	}
	identity, ok := identityFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
		return
	}
	grant := security.TicketGrant{}
	if identity.Principal != nil {
		scopes := strings.Join(identity.Principal.Scopes, " ")
		grant.Scopes, grant.Subject = &scopes, identity.Principal.Subject
	} else if identity.Session != nil {
		scopes := identity.Session.Scope
		grant.Scopes, grant.Subject = &scopes, identity.Session.Subject
	} else if !identity.StaticToken {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated identity is unavailable")
		return
	}
	ticket, err := s.Tickets.Issue(grant)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "too many outstanding WebSocket tickets; retry shortly\n")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket": ticket, "expires_in_secs": int(security.TicketTTL.Seconds()),
		"usage": "append as ?ticket=<value> to the WebSocket URL; valid once",
	})
}

func (s *Server) sensingSocket(w http.ResponseWriter, r *http.Request) {
	s.streamSocket(w, r, "sensing")
}
func (s *Server) poseSocket(w http.ResponseWriter, r *http.Request) { s.streamSocket(w, r, "pose") }
func (s *Server) introspectionSocket(w http.ResponseWriter, r *http.Request) {
	s.streamSocket(w, r, "introspection")
}
func (s *Server) trainingSocket(w http.ResponseWriter, r *http.Request) {
	s.streamSocket(w, r, "training")
}

func (s *Server) fieldSurface(w http.ResponseWriter, _ *http.Request) {
	if s.Field == nil {
		writeError(w, http.StatusServiceUnavailable, "rufield_unavailable", "the RuField signing surface could not initialize")
		return
	}
	writeJSON(w, http.StatusOK, s.Field.Snapshot())
}

func (s *Server) fieldSocket(w http.ResponseWriter, r *http.Request) {
	if s.Field == nil {
		writeError(w, http.StatusServiceUnavailable, "rufield_unavailable", "the RuField signing surface could not initialize")
		return
	}
	s.streamSocket(w, r, "field")
}

func (s *Server) streamSocket(w http.ResponseWriter, r *http.Request, topic string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeSocket(w, r) || !s.validateSocketOrigin(w, r) {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "stream closed")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	type inboundMessage struct {
		typeID websocket.MessageType
		data   []byte
		err    error
	}
	incoming := make(chan inboundMessage, 1)
	go func() {
		defer close(incoming)
		for {
			typeID, data, readErr := conn.Read(ctx)
			select {
			case incoming <- inboundMessage{typeID: typeID, data: data, err: readErr}:
			case <-ctx.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	if topic == "training" {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		lastPayload := ""
		publishStatus := func(eventType string) bool {
			status := training.Status{Enabled: false, Phase: "unavailable"}
			if s.Training != nil {
				status = s.Training.Status()
			}
			encoded, marshalErr := json.Marshal(status)
			if marshalErr != nil {
				return false
			}
			if string(encoded) == lastPayload {
				return true
			}
			if writeJSONFrame(ctx, conn, map[string]any{"type": eventType, "data": status}) != nil {
				return false
			}
			lastPayload = string(encoded)
			return true
		}
		if !publishStatus("status") {
			return
		}
		for {
			select {
			case message, ok := <-incoming:
				if !ok || message.err != nil {
					return
				}
				if message.typeID == websocket.MessageText && isJSONPing(message.data) {
					if writeJSONFrame(ctx, conn, map[string]string{"type": "pong"}) != nil {
						return
					}
				}
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !publishStatus("progress") {
					return
				}
			}
		}
	}
	var sub *stream.Subscription
	if topic == "field" {
		sub = s.Field.Subscribe()
	} else if topic == "sensing" || topic == "pose" {
		sub = s.Hub.SubscribeClient()
	} else {
		sub = s.Hub.Subscribe()
	}
	if sub == nil {
		writeError(w, http.StatusServiceUnavailable, "stream_unavailable", "the requested stream is unavailable")
		return
	}
	defer sub.Close()
	if topic == "pose" {
		if writeJSONFrame(ctx, conn, map[string]any{
			"type":    "connection_established",
			"payload": map[string]string{"status": "connected", "backend": "go"},
		}) != nil {
			return
		}
	}
	var pingTicker *time.Ticker
	var pingC <-chan time.Time
	if topic == "sensing" || topic == "pose" || topic == "field" {
		pingTicker = time.NewTicker(30 * time.Second)
		pingC = pingTicker.C
		defer pingTicker.Stop()
	}
	for {
		select {
		case message, ok := <-incoming:
			if !ok || message.err != nil {
				return
			}
			if message.typeID == websocket.MessageText && isJSONPing(message.data) {
				if writeJSONFrame(ctx, conn, map[string]string{"type": "pong"}) != nil {
					return
				}
			}
		case <-ctx.Done():
			return
		case <-pingC:
			pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			pingErr := conn.Ping(pingCtx)
			pingCancel()
			if pingErr != nil {
				return
			}
		case frame, ok := <-sub.C:
			if !ok {
				return
			}
			if s.Config.PrivacyMode && topic != "field" {
				filtered, filterErr := security.RedactJSON(frame, 16<<20)
				if filterErr != nil {
					return
				}
				frame = filtered
			}
			if topic != "sensing" && topic != "field" {
				frame, err = streamProjection(topic, frame)
				if err != nil {
					return
				}
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
			writeErr := conn.Write(writeCtx, websocket.MessageText, frame)
			writeCancel()
			if writeErr != nil {
				return
			}
		}
	}
}

func isJSONPing(data []byte) bool {
	var message struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(data, &message) == nil && message.Type == "ping"
}

func (s *Server) authorizeSocket(w http.ResponseWriter, r *http.Request) bool {
	if !s.authEnabled() {
		return true
	}
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		if grant, valid := s.Tickets.Consume(ticket); valid && grantAllowsRead(grant) {
			return true
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token, browser session, or single-use ticket required")
		return false
	}
	provided := security.BearerToken(r.Header.Get("Authorization"))
	if provided != "" {
		if security.EqualToken(s.Config.APIToken, provided) {
			return true
		}
		if s.OAuth != nil {
			if _, err := s.OAuth.VerifyAccessToken(r.Context(), provided, "sensing:read"); err == nil {
				return true
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
		return false
	}
	if s.OAuth != nil {
		if session := s.OAuth.Session(r.Header.Get("Cookie")); session != nil && session.HasScope("sensing:read") {
			return true
		}
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token, browser session, or single-use ticket required")
	return false
}

func grantAllowsRead(grant security.TicketGrant) bool {
	if grant.Scopes == nil {
		return true
	}
	for _, scope := range strings.Fields(*grant.Scopes) {
		if scope == "sensing:read" {
			return true
		}
	}
	return false
}

func (s *Server) validateSocketOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || s.Hosts == nil || !s.Hosts.Allows(parsed.Host) {
		writeError(w, http.StatusForbidden, "origin_rejected", "WebSocket origin is not allowed")
		return false
	}
	return true
}

func streamProjection(topic string, frame []byte) ([]byte, error) {
	var sensing state.SensingUpdate
	if err := json.Unmarshal(frame, &sensing); err != nil {
		return nil, err
	}
	switch topic {
	case "pose":
		confidence := 0.0
		if sensing.Classification.Presence {
			confidence = sensing.Classification.Confidence
		}
		return json.Marshal(map[string]any{
			"type": "pose_data", "zone_id": "zone_1", "timestamp": sensing.Timestamp,
			"physics": nil,
			"payload": map[string]any{
				"pose":        map[string]any{"persons": []any{}},
				"confidence":  confidence,
				"activity":    sensing.Classification.MotionLevel,
				"pose_source": "unavailable",
				"metadata": map[string]any{
					"frame_id": fmt.Sprintf("go_frame_%d", sensing.Tick), "processing_time_ms": 0,
					"source": sensing.Source, "tick": sensing.Tick,
					"signal_strength":      sensing.Features.MeanRSSI,
					"motion_band_power":    sensing.Features.MotionBandPower,
					"breathing_band_power": sensing.Features.BreathingBandPower,
					"estimated_persons":    0, "pose_available": false,
					"localization_available": false,
				},
			},
		})
	case "introspection":
		return json.Marshal(introspectionSnapshot(sensing))
	default:
		return nil, errors.New("unknown stream projection")
	}
}

// introspectionSnapshot preserves the Rust API's stable wire schema. Go does
// not currently port the attractor/DTW analyzer, so it reports the honest
// baseline state (`unknown`, no exponent, zero confidence) rather than
// synthesizing an analysis result.
func introspectionSnapshot(sensing state.SensingUpdate) map[string]any {
	timestampNS := uint64(0)
	if sensing.Timestamp > 0 && sensing.Timestamp < 1e10 {
		timestampNS = uint64(math.Round(sensing.Timestamp * 1e9))
	}
	return map[string]any{
		"timestamp_ns": timestampNS, "frame_count": sensing.Tick, "regime": "unknown",
		"lyapunov_exponent": nil, "attractor_dim": 3, "attractor_confidence": 0.0,
		"regime_changed": false, "top_k_similarity": []any{},
	}
}

func (s *Server) bearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() || !requiresBearer(r.URL.Path) || isWebSocketRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		provided := security.BearerToken(r.Header.Get("Authorization"))
		requiredScope := security.RequiredScope(r.Method, r.URL.Path)
		if security.EqualToken(s.Config.APIToken, provided) {
			identity := requestIdentity{StaticToken: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIdentityKey{}, identity)))
			return
		}
		if provided != "" && s.OAuth != nil {
			principal, err := s.OAuth.VerifyAccessToken(r.Context(), provided, requiredScope)
			if err != nil {
				if errors.Is(err, oauth.ErrMissingScope) {
					writeError(w, http.StatusForbidden, "insufficient_scope", "the OAuth token does not grant the required capability")
				} else {
					writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
				}
				return
			}
			identity := requestIdentity{Principal: &principal}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIdentityKey{}, identity)))
			return
		}
		if s.OAuth != nil {
			session := s.OAuth.Session(r.Header.Get("Cookie"))
			if session != nil {
				if !session.HasScope(requiredScope) {
					writeError(w, http.StatusForbidden, "insufficient_scope", "the browser session does not grant the required capability")
					return
				}
				identity := requestIdentity{Session: session}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIdentityKey{}, identity)))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token or browser session required")
	})
}

func requiresBearer(path string) bool {
	return strings.HasPrefix(path, "/api/v1/") || path == "/api/field"
}

func isWebSocketRoute(path string) bool {
	return strings.HasPrefix(path, "/ws/") || path == "/api/v1/stream/pose"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]string{"code": code, "detail": detail})
}

func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

type observedResponseWriter struct {
	http.ResponseWriter
	status  int
	started bool
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.status, w.started = status, true
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(body []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *observedResponseWriter) Flush() {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	connection, buffered, err := hijacker.Hijack()
	if err == nil {
		w.status, w.started = http.StatusSwitchingProtocols, true
	}
	return connection, buffered, err
}

func (w *observedResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func observeRequestsMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		observed := &observedResponseWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("http handler panic", "method", r.Method, "path", r.URL.Path, "panic_type", fmt.Sprintf("%T", recovered))
				if !observed.started {
					writeError(observed, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}
			status := observed.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.Info("http request", "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds())
		}()
		next.ServeHTTP(observed, r)
	})
}

func writeJSONFrame(parent context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}
