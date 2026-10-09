package config

import (
	"reflect"
	"testing"
)

func TestNodePositionsParseIdentityAndLegacyForms(t *testing.T) {
	got, err := parseNodePositions("5:0,1,2;9:3.5,-2,4;0,0,0")
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint8][3]float64{
		5: {0, 1, 2}, 9: {3.5, -2, 4}, 2: {0, 0, 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("positions=%v, want %v", got, want)
	}
}

func TestNodePositionsLoadFromEnvironmentAndFlag(t *testing.T) {
	cfg, err := Load(nil, func(key string) string {
		if key == "SENSING_NODE_POSITIONS" {
			return "5:0,0,0;9:4,0,0"
		}
		return ""
	})
	if err != nil || cfg.NodePositions[5] != [3]float64{0, 0, 0} || cfg.NodePositions[9] != [3]float64{4, 0, 0} {
		t.Fatalf("environment positions=%v err=%v", cfg.NodePositions, err)
	}
	cfg, err = Load([]string{"--node-positions", "12:1,2,3"}, func(key string) string {
		if key == "SENSING_NODE_POSITIONS" {
			return "5:0,0,0"
		}
		return ""
	})
	if err != nil || len(cfg.NodePositions) != 1 || cfg.NodePositions[12] != [3]float64{1, 2, 3} {
		t.Fatalf("flag positions=%v err=%v", cfg.NodePositions, err)
	}
}

func TestNodePositionsRejectAmbiguousOrUnsafeEntries(t *testing.T) {
	for _, spec := range []string{
		"5:0,0", "256:1,2,3", "5:0,NaN,2", "5:0,Inf,2",
		"5:0,0,0;5:1,1,1",
		"5:1000001,0,0",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, err := parseNodePositions(spec); err == nil {
				t.Fatalf("unsafe node positions %q were accepted", spec)
			}
		})
	}
}
