package localize

import (
	"math"
	"testing"
)

func TestExtractPeaksMapsStrongestFieldCellToDisplayScene(t *testing.T) {
	values := make([]float64, 20*20)
	values[4*20+15] = 1
	peaks := ExtractPeaks(values, 20, 20, 1, 3)
	if len(peaks) != 1 || peaks[0].Cell != [2]int{15, 4} {
		t.Fatalf("peaks=%+v", peaks)
	}
	if peaks[0].ScenePosition != [3]float64{3, 0, -3} {
		t.Fatalf("scene position=%v", peaks[0].ScenePosition)
	}
}

func TestExtractPeaksFiltersWeakAndNearbyCells(t *testing.T) {
	values := make([]float64, 20*20)
	values[10*20+10] = 1
	values[10*20+11] = 0.99
	values[2*20+3] = 0.9
	values[0] = 0.1
	peaks := ExtractPeaks(values, 20, 20, 3, 3)
	if len(peaks) != 2 || peaks[0].Cell != [2]int{10, 10} || peaks[1].Cell != [2]int{3, 2} {
		t.Fatalf("unexpected separated peaks=%+v", peaks)
	}
}

func TestExtractPeaksRejectsInvalidBoundsAndNonFiniteValues(t *testing.T) {
	if got := ExtractPeaks(make([]float64, 1), math.MaxInt, 2, 1, 0); len(got) != 0 {
		t.Fatalf("overflowing grid returned peaks: %+v", got)
	}
	values := []float64{math.NaN(), math.Inf(1), 0.8, 0.7}
	peaks := ExtractPeaks(values, 2, 2, 5, -1)
	if len(peaks) != 2 {
		t.Fatalf("non-finite values were not ignored: %+v", peaks)
	}
}
