package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Space coordinates are operator configuration, never RF localization results.
type spaceDevice struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	NodeID   *uint8    `json:"node_id,omitempty"`
	Label    string    `json:"label"`
	Position []float64 `json:"position"`
}
type spaceLayout struct {
	Revision uint64        `json:"revision"`
	Devices  []spaceDevice `json:"devices"`
}

func validateSpace(layout spaceLayout) error {
	if layout.Devices == nil || len(layout.Devices) > 64 {
		return errors.New("devices must be an array of at most 64 entries")
	}
	ids, nodes := map[string]bool{}, map[uint8]bool{}
	for _, device := range layout.Devices {
		if len(device.ID) == 0 || len(device.ID) > 48 || strings.ContainsFunc(device.ID, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) || ids[device.ID] {
			return errors.New("device IDs must be unique ASCII identifiers")
		}
		ids[device.ID] = true
		if !utf8.ValidString(device.Label) || strings.TrimSpace(device.Label) == "" || len(device.Label) > 80 || strings.ContainsFunc(device.Label, unicode.IsControl) {
			return errors.New("labels must be non-empty text of at most 80 bytes")
		}
		if device.Kind != "esp32" && device.Kind != "router" {
			return errors.New("kind must be esp32 or router")
		}
		if device.Kind == "esp32" {
			if device.NodeID == nil || nodes[*device.NodeID] {
				return errors.New("ESP32 devices require a unique node_id")
			}
			nodes[*device.NodeID] = true
		} else if device.NodeID != nil {
			return errors.New("routers cannot have an ESP32 node_id")
		}
		if len(device.Position) != 3 {
			return errors.New("positions require exactly x, y, z")
		}
		for _, v := range device.Position {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 10000 {
				return errors.New("coordinates must be finite metres within -10000..10000")
			}
		}
	}
	return nil
}

func (s *Server) spacePath() string { return filepath.Join(s.Config.DataDir, "space-layout.json") }
func (s *Server) applySpace(layout spaceLayout) {
	positions := map[uint8][3]float64{}
	for _, d := range layout.Devices {
		if d.NodeID != nil {
			positions[*d.NodeID] = [3]float64(d.Position)
		}
	}
	// validateSpace has already checked finiteness.
	if err := s.State.SetNodePositions(positions); err != nil {
		s.spaceErr = err
		return
	}
	s.spaceLayout = layout
}
func (s *Server) initSpace() {
	layout := spaceLayout{Devices: []spaceDevice{}}
	for id, position := range s.Config.NodePositions {
		nodeID := id
		layout.Devices = append(layout.Devices, spaceDevice{ID: fmt.Sprintf("esp32-%d", id), Kind: "esp32", NodeID: &nodeID, Label: fmt.Sprintf("ESP32 %d", id), Position: []float64{position[0], position[1], position[2]}})
	}
	sort.Slice(layout.Devices, func(i, j int) bool { return layout.Devices[i].ID < layout.Devices[j].ID })
	if strings.TrimSpace(s.Config.DataDir) != "" {
		file, err := os.Open(s.spacePath())
		if err == nil {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil || info.Size() > 64*1024 {
				s.spaceErr = errors.New("stored layout exceeds size limit")
				return
			}
			dec := json.NewDecoder(io.LimitReader(file, 64*1024))
			dec.DisallowUnknownFields()
			err = dec.Decode(&layout)
			if err == nil {
				var extra any
				if dec.Decode(&extra) != io.EOF {
					err = errors.New("trailing layout data")
				}
			}
			if err == nil {
				err = validateSpace(layout)
			}
		}
		if err != nil && !os.IsNotExist(err) {
			s.spaceErr = err
			return
		}
	}
	if err := validateSpace(layout); err != nil {
		s.spaceErr = err
		return
	}
	s.applySpace(layout)
}
func (s *Server) getSpace(w http.ResponseWriter, r *http.Request) {
	s.spaceMu.Lock()
	defer s.spaceMu.Unlock()
	if s.spaceErr != nil {
		writeError(w, 503, "space_unavailable", "stored device layout is invalid; restore the layout file before editing")
		return
	}
	writeJSON(w, 200, map[string]any{
		"layout": s.spaceLayout, "coordinate_frame": "installation_local", "units": "metres", "vertical_axis": "y",
		"position_source": "operator_configured", "person_localization_available": false, "animal_detection_available": false,
	})
}
func (s *Server) saveSpace(w http.ResponseWriter, r *http.Request) {
	var layout spaceLayout
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if !decodeJSONBody(w, r, &layout) {
		return
	}
	if err := validateSpace(layout); err != nil {
		writeError(w, 400, "invalid_layout", err.Error())
		return
	}
	s.spaceMu.Lock()
	defer s.spaceMu.Unlock()
	if s.spaceErr != nil || strings.TrimSpace(s.Config.DataDir) == "" {
		writeError(w, 503, "space_unavailable", "persistent device layout is unavailable")
		return
	}
	if layout.Revision != s.spaceLayout.Revision {
		writeError(w, 409, "layout_changed", "device layout changed; refresh before saving")
		return
	}
	layout.Revision++
	data, err := json.Marshal(layout)
	if err == nil {
		var temp *os.File
		temp, err = os.CreateTemp(s.Config.DataDir, ".space-layout-*")
		if err == nil {
			defer os.Remove(temp.Name())
			if err = temp.Chmod(0600); err == nil {
				_, err = temp.Write(data)
			}
			if err == nil {
				err = temp.Sync()
			}
			closeErr := temp.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Rename(temp.Name(), s.spacePath())
			}
		}
	}
	if err != nil {
		writeError(w, 500, "layout_save_failed", "could not persist device layout")
		return
	}
	s.applySpace(layout)
	writeJSON(w, 200, map[string]any{"layout": layout, "position_source": "operator_configured"})
}
