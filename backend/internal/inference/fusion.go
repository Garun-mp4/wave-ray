// Package inference contains deterministic freshness-aware room fusion.
package inference

import (
	"math"
	"sort"
)

type Node struct {
	Classification string
	Confidence     float64
	AgeMS          *uint64
}

type Room struct {
	Classification    string
	Confidence        float64
	ContributingNodes int
}

func (n Node) FreshnessWeight(staleAfterMS uint64) float64 {
	if staleAfterMS == 0 || n.AgeMS == nil || *n.AgeMS >= staleAfterMS {
		return 0
	}
	return float64(staleAfterMS-*n.AgeMS) / float64(staleAfterMS)
}

func FuseRoom(nodes []Node, staleAfterMS uint64) Room {
	type tally struct {
		weight       float64
		weightedConf float64
	}
	byClass := make(map[string]tally)
	contributing := 0
	for _, node := range nodes {
		weight := node.FreshnessWeight(staleAfterMS)
		if weight <= 0 {
			continue
		}
		classification := node.Classification
		if classification == "" {
			classification = "unavailable"
		}
		confidence := node.Confidence
		if math.IsNaN(confidence) || math.IsInf(confidence, 0) {
			confidence = 0
		}
		confidence = clamp(confidence, 0, 1)
		current := byClass[classification]
		current.weight += weight
		current.weightedConf += weight * confidence
		byClass[classification] = current
		contributing++
	}
	if contributing == 0 {
		return Room{Classification: "unavailable"}
	}
	classes := make([]string, 0, len(byClass))
	for classification := range byClass {
		classes = append(classes, classification)
	}
	sort.Strings(classes)
	winner := classes[0]
	for _, classification := range classes[1:] {
		if byClass[classification].weight > byClass[winner].weight {
			winner = classification
		}
	}
	winning := byClass[winner]
	confidence := 0.0
	if winning.weight > 0 {
		confidence = clamp(winning.weightedConf/winning.weight, 0, 1)
	}
	return Room{Classification: winner, Confidence: confidence, ContributingNodes: contributing}
}

func Classification(room Room) (string, bool, float64) {
	if room.Classification == "unavailable" || room.Classification == "" {
		return "absent", false, 0
	}
	return room.Classification, room.Classification != "absent", clamp(room.Confidence, 0, 1)
}

func clamp(value, low, high float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return low
	}
	return math.Max(low, math.Min(high, value))
}
