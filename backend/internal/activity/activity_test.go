package activity

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFlatSignalHasNeutralRelativeIndex(t *testing.T) {
	start := time.Now()
	tracker := &Tracker{}
	verdict := tracker.Observe(start, 0, 4)
	if verdict.Index != 0 || math.Abs(verdict.Level-0.4) > 1e-12 || verdict.AbsLevel != 0 || verdict.Floor != SlowFloor {
		t.Fatalf("flat signal should map to the neutral index: %+v", verdict)
	}
}

func TestRelativeIndexRespondsToStepAndSlowlyAdapts(t *testing.T) {
	start := time.Now()
	tracker := &Tracker{}
	tracker.Observe(start, 0.05, 20)
	for i := 1; i <= 5; i++ {
		tracker.Observe(start.Add(time.Duration(i)*SlowTimeConstant), 0.05, 20)
	}
	baseline := tracker.Observe(start.Add(5*SlowTimeConstant+100*time.Millisecond), 0.5, 20)
	if baseline.Index < IndexMax-0.1 {
		t.Fatalf("10x step should reach the upper index bound, got %+v", baseline)
	}
	adapted := tracker.Observe(start.Add(5*SlowTimeConstant+300*time.Second), 0.5, 20)
	if adapted.Index >= baseline.Index {
		t.Fatalf("index should decay as the slow baseline adapts: before=%+v after=%+v", baseline, adapted)
	}
}

func TestFloorBootstrapsFromWindowAndSeedSkipsBootstrap(t *testing.T) {
	start := time.Now()
	tracker := &Tracker{}
	first := tracker.Observe(start, 0.01, 10)
	if first.Floor <= 0 || first.AbsLevel >= 0.3 {
		t.Fatalf("provisional floor should keep a quiet first reading near zero: %+v", first)
	}
	tracker.Observe(start.Add(15*time.Second), 0.01, 10)
	committed := tracker.Observe(start.Add(FloorInitWindow), 0.01, 10)
	if _, ok := tracker.Floor(); !ok || committed.AbsLevel >= 0.3 {
		t.Fatalf("quiet-room floor was not committed safely: %+v", committed)
	}

	seeded := &Tracker{}
	if !seeded.SeedFloor(0.02) {
		t.Fatal("positive persisted floor was rejected")
	}
	verdict := seeded.Observe(start, 0.02, 1)
	if math.Abs(verdict.Floor-0.02) > 1e-12 || verdict.AbsLevel >= 0.1 {
		t.Fatalf("seeded floor should be active immediately: %+v", verdict)
	}
	if seeded.SeedFloor(math.NaN()) {
		t.Fatal("non-finite floor must be rejected")
	}
}

func TestManagerHistoryIsDecimatedAndReturnsBoundedCopies(t *testing.T) {
	manager := NewManager()
	start := time.Now()
	manager.Observe("device-a", start, 0.02, 10)
	manager.Observe("device-a", start.Add(500*time.Millisecond), 0.2, 15)
	if got := manager.History(start.Add(500*time.Millisecond), 300); len(got) != 1 {
		t.Fatalf("sub-second ingest should not add a history sample: %+v", got)
	}
	manager.Observe("device-b", start.Add(time.Second), 0.03, 5)
	samples := manager.History(start.Add(1500*time.Millisecond), 300)
	if len(samples) != 2 || samples[0].AgeMS != 1500 || samples[1].AgeMS != 500 {
		t.Fatalf("history sample ages/order mismatch: %+v", samples)
	}
	if _, ok := samples[1].Devices["device-a"]; !ok {
		t.Fatalf("fresh device missing from sampled history: %+v", samples[1])
	}
	samples[1].Devices["device-a"] = DeviceLevel{Level: 99}
	again := manager.History(start.Add(1500*time.Millisecond), 300)
	if again[1].Devices["device-a"].Level == 99 {
		t.Fatal("caller mutated retained activity history")
	}
	if got := manager.History(start.Add(1500*time.Millisecond), 0); len(got) != 0 {
		t.Fatalf("zero-second history should be empty when sample age is positive: %+v", got)
	}
}

func TestManagerDeviceRegistryStaysBounded(t *testing.T) {
	manager := NewManager()
	start := time.Now()
	for i := 0; i <= maxDevices; i++ {
		manager.Observe(string(rune('a'+i)), start.Add(time.Duration(i)*time.Millisecond), 0.01, 1)
	}
	if got := len(manager.devices); got != maxDevices {
		t.Fatalf("device registry exceeded bound: got=%d want=%d", got, maxDevices)
	}
}

func TestActivityFloorPersistenceIsPrivateBoundedAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "activity-state.json")
	if floors, err := LoadFloors(path); err != nil || len(floors) != 0 {
		t.Fatalf("missing state should mean first run: floors=%v err=%v", floors, err)
	}
	want := map[string]float64{"node-a": 0.02, "node-b": 0.03}
	if err := WriteFloors(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFloors(path)
	if err != nil || len(got) != len(want) || got["node-a"] != want["node-a"] || got["node-b"] != want["node-b"] {
		t.Fatalf("persisted floors mismatch: got=%v err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions=%o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("atomic state write left temporary files: %v", entries)
	}

	for name, contents := range map[string]string{
		"null":            `null`,
		"trailing":        `{"node-a":0.1} {}`,
		"invalid floor":   `{"node-a":0}`,
		"invalid ID":      `{"":0.1}`,
		"wrong JSON type": `{"node-a":"quiet"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFloors(path); err == nil {
				t.Fatalf("invalid state accepted: %s", contents)
			}
		})
	}
	if err := WriteFloors(path, map[string]float64{"node-a": math.NaN()}); err == nil {
		t.Fatal("non-finite floor was persisted")
	}
}
