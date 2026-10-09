package pose

import (
	"testing"
	"time"
)

func TestTrackerAssociatesMultipleDetectionsIndependentOfInputOrder(t *testing.T) {
	tracker, err := NewTracker(0.25)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(300, 0)
	tracks, err := tracker.Update(base, []Detection{
		{Center: [2]float64{0.1, 0.2}, Space: "normalized_image"},
		{Center: [2]float64{0.8, 0.7}, Space: "normalized_image"},
	})
	if err != nil || len(tracks) != 2 {
		t.Fatalf("initial tracks=%+v err=%v", tracks, err)
	}
	firstIDs := [2]uint64{tracks[0].ID, tracks[1].ID}
	tracks, err = tracker.Update(base.Add(50*time.Millisecond), []Detection{
		{Center: [2]float64{0.79, 0.71}, Space: "normalized_image"},
		{Center: [2]float64{0.11, 0.19}, Space: "normalized_image"},
	})
	if err != nil || len(tracks) != 2 {
		t.Fatalf("updated tracks=%+v err=%v", tracks, err)
	}
	if tracks[0].ID != firstIDs[0] || tracks[1].ID != firstIDs[1] || !tracks[0].Confirmed || !tracks[1].Confirmed {
		t.Fatalf("track IDs changed after detection order changed: first=%v tracks=%+v", firstIDs, tracks)
	}
}

func TestTrackerNeverAssociatesAcrossCoordinateSpaces(t *testing.T) {
	tracker, _ := NewTracker(1)
	base := time.Unix(400, 0)
	first, _ := tracker.Update(base, []Detection{{Center: [2]float64{0.5, 0.5}, Space: "normalized_image"}})
	second, err := tracker.Update(base.Add(50*time.Millisecond), []Detection{{Center: [2]float64{0.5, 0.5}, Space: "room_floor_m"}})
	if err != nil || len(second) != 2 || second[0].ID != first[0].ID || second[0].Missed != 1 || second[1].ID == first[0].ID {
		t.Fatalf("image and metric coordinates were incorrectly associated: first=%+v second=%+v err=%v", first, second, err)
	}
}
