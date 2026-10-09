package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ruvnet/ruview/backend/internal/calibration"
)

type calibrationIdentityRequest struct {
	BootEpoch     string  `json:"boot_epoch"`
	SessionID     *string `json:"session_id"`
	BindingDigest *string `json:"binding_digest"`
	SourceNodeIDs []int   `json:"source_node_ids"`
}

func (request calibrationIdentityRequest) identity() calibration.Identity {
	return calibration.Identity{
		BootEpoch: request.BootEpoch, SessionID: request.SessionID,
		BindingDigest: request.BindingDigest, SourceNodeIDs: request.SourceNodeIDs,
	}
}

func (s *Server) calibrationStart(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request calibration.StartRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	var source *uint8
	if raw := r.URL.Query().Get("source_node_id"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 8)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error_code": "invalid_source_node_id",
				"error": "source_node_id must be an unsigned 8-bit integer",
			})
			return
		}
		parsed := uint8(value)
		source = &parsed
	}
	result, err := s.Calibration.Start(request, source)
	if err != nil {
		if errors.Is(err, calibration.ErrSourceUnavailable) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false, "error_code": "calibration_source_required",
				"error":            "No bound ESP32 node has a qualifying raw CSI grid. Start CSI streaming and retry after at least 10 seconds.",
				"eligible_sources": calibrationCandidates(s.Calibration, request.SourceNodeIDs, time.Now()),
			})
			return
		}
		if errors.Is(err, calibration.ErrGridUnavailable) && source != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false, "error_code": "calibration_grid_unavailable",
				"error":          "The requested source has no CSI grid with at least 10 seconds of evidence, at least 2 Hz, and gaps below 5 seconds.",
				"source_node_id": *source, "candidates": s.Calibration.Candidates(*source, time.Now()),
			})
			return
		}
		writeCalibrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func calibrationCandidates(manager *calibration.Manager, nodeIDs []int, now time.Time) []map[string]any {
	result := make([]map[string]any, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		result = append(result, map[string]any{"source_node_id": nodeID, "candidates": manager.Candidates(uint8(nodeID), now)})
	}
	return result
}

func (s *Server) calibrationStop(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request calibrationIdentityRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	result, err := s.Calibration.Stop(request.identity(), time.Now())
	writeCalibrationResult(w, result, err)
}

func (s *Server) calibrationCancel(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request calibrationIdentityRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	result, err := s.Calibration.Cancel(request.identity())
	writeCalibrationResult(w, result, err)
}

func (s *Server) calibrationReset(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request calibrationIdentityRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	result, err := s.Calibration.Reset(request.identity())
	writeCalibrationResult(w, result, err)
}

func (s *Server) calibrationStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	status := s.Calibration.Status(time.Now())
	status["trust"] = s.measurementTrust(time.Now())
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) poseCalibrationStatus(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	pose := s.State.PoseInference(now)
	status := map[string]any{
		"pose_calibrated": false, "spatial_localization_available": false,
		"person_localization_available": false, "message": "A calibrated transform of pose_v1 outputs can be stored, but model quality is not accepted for person localization.",
		"pose_inference": pose,
	}
	if s.Calibration != nil {
		status["trust"] = s.measurementTrust(now)
	} else {
		status["trust"] = map[string]any{"available": false, "reason": "room_baseline_calibration_unavailable"}
	}
	if s.Calibration != nil {
		status["room_csi_calibration"] = s.Calibration.Status(now)
	} else if s.calibrationErr != nil {
		status["room_csi_calibration_error"] = "room baseline calibration is unavailable"
	}
	if s.PoseMetric != nil {
		metric := s.PoseMetric.Status()
		status["metric_transform_available"] = metric.Available
		status["metric_calibration"] = metric
		status["message"] = metric.Reason
	} else {
		status["metric_transform_available"] = false
		status["metric_calibration"] = nil
		status["metric_calibration_error"] = "metric pose calibration is unavailable"
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) trustStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	writeJSON(w, http.StatusOK, s.measurementTrust(time.Now()))
}

func (s *Server) measurementTrust(now time.Time) map[string]any {
	update, status, _ := s.State.Latest(now)
	var occupants *uint8
	var quality *float64
	if status == "fresh" {
		occupants = update.EstimatedPersons
		quality = update.SignalQualityScore
	}
	confidence := s.State.LatestFusionConfidence(now)
	return s.Calibration.Trust(occupants, quality, confidence, now)
}

func (s *Server) vitalSigns(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	update, _, _ := s.State.Latest(now)
	trust := s.measurementTrust(now)
	reason := "calibrated measurement requirements are not met"
	if reasons, ok := trust["reasons"].([]string); ok && len(reasons) > 0 {
		reason = reasons[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vital_signs": nil, "authority": "abstained", "abstention_reason": reason,
		"trust": trust, "source": update.Source, "tick": update.Tick,
	})
}

func (s *Server) edgeVitals(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "abstained", "edge_vitals": nil,
		"message": "Raw edge vital values are withheld until a calibrated inference path can authorize them.",
		"trust":   s.measurementTrust(time.Now()),
	})
}

func (s *Server) dedupConfig(w http.ResponseWriter, _ *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dedup_factor": s.Calibration.DedupFactor(), "available": true,
		"range":                   []float64{1, 10},
		"description":             "Persisted divisor applied to the fresh per-node count-sum fallback. This is not spatial CSI fusion.",
		"applied_to_count_fusion": true,
	})
}

func (s *Server) setDedupConfig(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request struct {
		Value *float64 `json:"value"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Value == nil {
		writeError(w, http.StatusBadRequest, "invalid_dedup_factor", "value must be a finite number between 1 and 10")
		return
	}
	value, err := s.Calibration.SetDedupFactor(*request.Value)
	if err != nil {
		writeCalibrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "dedup_factor": value, "applied_to_count_fusion": true})
}

func (s *Server) setGroundTruth(w http.ResponseWriter, r *http.Request) {
	if s.Calibration == nil {
		writeCalibrationUnavailable(w, s.calibrationErr)
		return
	}
	var request struct {
		Count uint64 `json:"count"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Count == 0 {
		writeError(w, http.StatusBadRequest, "invalid_ground_truth", "count must be a positive integer")
		return
	}
	rawSum, contributors := s.State.FreshPersonCountSum(time.Now(), 10*time.Second)
	factor, err := s.Calibration.SetGroundTruth(rawSum, request.Count)
	if err != nil {
		writeCalibrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "ground_truth": request.Count, "raw_sum": rawSum,
		"fresh_count_contributors": contributors, "computed_dedup_factor": factor,
		"applied_to_count_fusion": true,
		"note":                    "The factor is applied to the per-node count-sum fallback. Spatial CSI fusion and calibrated person-count evidence remain unavailable.",
	})
}

func writeCalibrationResult(w http.ResponseWriter, result map[string]any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	status, code, message := calibrationErrorDetails(err)
	if result == nil {
		result = make(map[string]any)
	}
	result["success"], result["error_code"], result["error"] = false, code, message
	writeJSON(w, status, result)
}

func writeCalibrationError(w http.ResponseWriter, err error) {
	status, code, message := calibrationErrorDetails(err)
	writeJSON(w, status, map[string]any{"success": false, "error_code": code, "error": message})
}

func calibrationErrorDetails(err error) (int, string, string) {
	switch {
	case errors.Is(err, calibration.ErrInvalidBinding):
		return http.StatusBadRequest, "calibration_binding_invalid", "Provide a lowercase SHA-256 room binding and 1 to 16 sorted unique source node IDs."
	case errors.Is(err, calibration.ErrIdentityMismatch):
		return http.StatusConflict, "calibration_identity_mismatch", "The request does not match the active boot, session, room binding, and node set."
	case errors.Is(err, calibration.ErrAlreadyActive):
		return http.StatusConflict, "calibration_already_active", "Calibration is already in progress; stop or cancel the active session first."
	case errors.Is(err, calibration.ErrFreshModel):
		return http.StatusConflict, "calibration_model_fresh", "A fresh calibration model already exists; reset it or wait for expiry before recalibrating."
	case errors.Is(err, calibration.ErrCalibrationExpired):
		return http.StatusConflict, "calibration_model_expired", "The saved calibration has expired; start a new capture after fresh CSI grid evidence is available."
	case errors.Is(err, calibration.ErrNotCollecting):
		return http.StatusConflict, "calibration_not_collecting", "Only an unfinished calibration capture can be cancelled."
	case errors.Is(err, calibration.ErrSequenceDiscontinuity):
		return http.StatusConflict, "calibration_sequence_discontinuity", "A bound CSI sequence moved backward beyond the bounded UDP reorder window. Cancel and restart the capture."
	case errors.Is(err, calibration.ErrMissingContributors):
		return http.StatusConflict, "calibration_source_nodes_missing", "Every bound device must contribute at least one forward CSI frame on the frozen grid before finalization."
	case errors.Is(err, calibration.ErrGridStale):
		return http.StatusConflict, "calibration_grid_stale", "The selected CSI grid has not produced a fresh frame within five seconds."
	case errors.Is(err, calibration.ErrInsufficientEvidence):
		return http.StatusConflict, "calibration_insufficient_evidence", "Keep the room empty until both the frame and ten-minute duration targets are met."
	case errors.Is(err, calibration.ErrInvalidGroundTruth):
		return http.StatusBadRequest, "invalid_ground_truth", "count must be a positive integer."
	default:
		return http.StatusInternalServerError, "calibration_operation_failed", "Calibration operation failed; check server logs for details."
	}
}

func writeCalibrationUnavailable(w http.ResponseWriter, cause error) {
	message := "Room calibration state is not configured."
	if cause != nil {
		message = "Room calibration state could not be loaded; calibration controls are disabled."
	}
	writeError(w, http.StatusServiceUnavailable, "calibration_unavailable", message)
}
