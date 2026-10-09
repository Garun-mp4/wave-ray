package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const maxNodePositionCoordinate = 1_000_000

// parseNodePositions accepts the Rust-compatible positional form
// "x,y,z;x,y,z" and the identity-safe form "node_id:x,y,z;...".
// Positional entries retain their list index as the node ID.
func parseNodePositions(spec string) (map[uint8][3]float64, error) {
	positions := make(map[uint8][3]float64)
	if strings.TrimSpace(spec) == "" {
		return positions, nil
	}
	for index, rawEntry := range strings.Split(spec, ";") {
		entry := strings.TrimSpace(rawEntry)
		if entry == "" {
			continue
		}
		nodeID := uint64(index)
		triplet := entry
		if idText, rest, explicit := strings.Cut(entry, ":"); explicit {
			parsed, err := strconv.ParseUint(strings.TrimSpace(idText), 10, 8)
			if err != nil {
				return nil, fmt.Errorf("node position entry %d has invalid node id: %w", index, err)
			}
			nodeID, triplet = parsed, rest
		} else if nodeID > 255 {
			return nil, fmt.Errorf("positional node position entry %d exceeds node id range 0..255", index)
		}
		parts := strings.Split(triplet, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("node position entry %d must contain exactly three coordinates", index)
		}
		var position [3]float64
		for coordinateIndex, part := range parts {
			value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > maxNodePositionCoordinate {
				return nil, fmt.Errorf("node position entry %d contains an invalid coordinate", index)
			}
			position[coordinateIndex] = value
		}
		id := uint8(nodeID)
		if _, exists := positions[id]; exists {
			return nil, fmt.Errorf("node position for node %d is configured more than once", id)
		}
		positions[id] = position
	}
	return positions, nil
}
