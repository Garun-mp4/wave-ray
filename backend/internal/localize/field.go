// Package localize maps the displayed signal-field grid to its scene coordinate
// system. The result is a signal hotspot, not a calibrated person position.
package localize

import (
	"math"
	"sort"
)

const (
	peakThreshold = 0.35
	xScale        = 0.6
	zScale        = 0.5
	maxGridCells  = 1 << 20
)

type Peak struct {
	ScenePosition [3]float64 `json:"scene_position"`
	Intensity     float64    `json:"intensity"`
	Cell          [2]int     `json:"cell"`
}

// ExtractPeaks returns deterministic, spatially separated high-energy cells
// from a row-major [z][x] field. Scene coordinates use the Observatory display
// scale and do not represent meters or calibrated room localization.
func ExtractPeaks(values []float64, nx, nz, maxPeaks int, minSeparationCells float64) []Peak {
	if nx <= 0 || nz <= 0 || nx > maxGridCells/nz || nx*nz > maxGridCells || len(values) < nx*nz || maxPeaks <= 0 {
		return []Peak{}
	}
	if maxPeaks > nx*nz {
		maxPeaks = nx * nz
	}
	if !isFinite(minSeparationCells) || minSeparationCells < 0 {
		minSeparationCells = 0
	}
	candidates := make([]Peak, 0)
	for iz := 0; iz < nz; iz++ {
		for ix := 0; ix < nx; ix++ {
			intensity := values[iz*nx+ix]
			if !isFinite(intensity) || intensity < peakThreshold {
				continue
			}
			candidates = append(candidates, Peak{
				ScenePosition: [3]float64{(float64(ix) - float64(nx)/2) * xScale, 0, (float64(iz) - float64(nz)/2) * zScale},
				Intensity:     intensity,
				Cell:          [2]int{ix, iz},
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Intensity == candidates[j].Intensity {
			if candidates[i].Cell[1] == candidates[j].Cell[1] {
				return candidates[i].Cell[0] < candidates[j].Cell[0]
			}
			return candidates[i].Cell[1] < candidates[j].Cell[1]
		}
		return candidates[i].Intensity > candidates[j].Intensity
	})

	peaks := make([]Peak, 0, maxPeaks)
	for _, candidate := range candidates {
		separated := true
		for _, peak := range peaks {
			dx := float64(peak.Cell[0] - candidate.Cell[0])
			dz := float64(peak.Cell[1] - candidate.Cell[1])
			if math.Hypot(dx, dz) < minSeparationCells {
				separated = false
				break
			}
		}
		if separated {
			peaks = append(peaks, candidate)
			if len(peaks) == maxPeaks {
				break
			}
		}
	}
	return peaks
}

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
