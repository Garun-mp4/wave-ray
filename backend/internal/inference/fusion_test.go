package inference

import (
	"math"
	"testing"
)

func age(value uint64) *uint64 { return &value }

func TestFuseRoomWeightsFreshnessAndConfidence(t *testing.T) {
	room := FuseRoom([]Node{
		{Classification: "present_moving", Confidence: 0.8, AgeMS: age(0)},
		{Classification: "present_moving", Confidence: 0.6, AgeMS: age(5_000)},
		{Classification: "absent", Confidence: 1, AgeMS: age(9_500)},
	}, 10_000)
	if room.Classification != "present_moving" || room.ContributingNodes != 3 || math.Abs(room.Confidence-(11.0/15.0)) > 1e-9 {
		t.Fatalf("unexpected freshness-weighted room: %+v", room)
	}
}

func TestFuseRoomIsOrderIndependentAndBreaksTiesLexically(t *testing.T) {
	a := Node{Classification: "present_moving", Confidence: 0.8, AgeMS: age(0)}
	b := Node{Classification: "absent", Confidence: 0.6, AgeMS: age(0)}
	first := FuseRoom([]Node{a, b}, 10_000)
	second := FuseRoom([]Node{b, a}, 10_000)
	if first != second || first.Classification != "absent" || first.Confidence != 0.6 {
		t.Fatalf("tie result is unstable or not lexical: first=%+v second=%+v", first, second)
	}
}

func TestFuseRoomExcludesMissingAndStaleNodes(t *testing.T) {
	room := FuseRoom([]Node{
		{Classification: "present_moving", Confidence: 1, AgeMS: nil},
		{Classification: "present_still", Confidence: 1, AgeMS: age(10_000)},
	}, 10_000)
	if room.Classification != "unavailable" || room.Confidence != 0 || room.ContributingNodes != 0 {
		t.Fatalf("stale nodes contributed to room: %+v", room)
	}
	classification, presence, confidence := Classification(room)
	if classification != "absent" || presence || confidence != 0 {
		t.Fatalf("unavailable mapping=%q presence=%t confidence=%f", classification, presence, confidence)
	}
}

func TestNodeConfidenceIsClampedAndNonFiniteInputIgnored(t *testing.T) {
	room := FuseRoom([]Node{
		{Classification: "present_still", Confidence: math.NaN(), AgeMS: age(0)},
		{Classification: "present_still", Confidence: 2, AgeMS: age(0)},
	}, 100)
	if room.Confidence != 0.5 {
		t.Fatalf("confidence was not safely clamped: %+v", room)
	}
}
