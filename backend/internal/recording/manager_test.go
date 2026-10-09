package recording

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruvnet/ruview/backend/internal/ws"
)

func TestRecordingLifecycleAndSyntheticWatermark(t *testing.T) {
	root := t.TempDir()
	hub := ws.NewHub()
	manager, err := New(root, hub, false)
	if err != nil {
		t.Fatal(err)
	}
	id, watermark, err := manager.Start("sample", "simulate")
	if err != nil || id != "sample" || watermark == nil || *watermark != "SYNTHETIC" {
		t.Fatalf("id=%s watermark=%v err=%v", id, watermark, err)
	}
	if _, _, err := manager.Start("second", "esp32"); err != ErrRecordingActive {
		t.Fatalf("second start error=%v", err)
	}
	hub.Publish([]byte(`{"type":"sensing_update","source":"simulate","presence":true}`))
	stoppedID, frames, _, err := manager.Stop()
	if err != nil || stoppedID != id || frames != 1 {
		t.Fatalf("stop: id=%s frames=%d err=%v", stoppedID, frames, err)
	}
	items, err := manager.List()
	if err != nil || len(items) != 1 || items[0].Frames != 1 || items[0].Watermark == nil || *items[0].Watermark != "SYNTHETIC" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "recordings", "sample.jsonl"))
	if err != nil || !strings.Contains(string(data), `"source":"simulate"`) || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("recording data=%s err=%v", data, err)
	}
	if err := manager.Delete("sample"); err != nil {
		t.Fatal(err)
	}
}

func TestPrivacyModeRedactsRecordedBiometrics(t *testing.T) {
	root := t.TempDir()
	hub := ws.NewHub()
	manager, err := New(root, hub, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Start("privacy", "esp32"); err != nil {
		t.Fatal(err)
	}
	hub.Publish([]byte(`{"type":"sensing_update","vital_signs":{"heart_rate_bpm":72},"presence":true}`))
	if _, _, _, err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "recordings", "privacy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "vital_signs") || strings.Contains(string(data), "heart_rate_bpm") || !strings.Contains(string(data), `"presence":true`) {
		t.Fatalf("unexpected privacy-filtered recording: %s", data)
	}
}

func TestRecordingIDAndDeleteSafety(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root, ws.NewHub(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Start("../escape", "esp32"); err != ErrInvalidID {
		t.Fatalf("path traversal ID error=%v", err)
	}
	if err := manager.Delete("missing"); err != ErrRecordingNotFound {
		t.Fatalf("missing recording error=%v", err)
	}
	if _, _, err := manager.Start("active", "esp32"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete("active"); err != ErrActiveDelete {
		t.Fatalf("active delete error=%v", err)
	}
	if _, _, _, err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestListBoundsCorruptRecordingLines(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root, ws.NewHub(), false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "recordings", "corrupt.jsonl")
	if err := os.WriteFile(path, make([]byte, maxFrameBytes+2), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "invalid" {
		t.Fatalf("expected corrupt recording to be marked invalid, got %+v", items)
	}
}

func TestDeleteAndStartSerializeSameRecordingID(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root, ws.NewHub(), false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "recordings", "same.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	startDone := make(chan error, 1)
	deleteDone := make(chan error, 1)
	start := make(chan struct{})
	go func() {
		<-start
		_, _, startErr := manager.Start("same", "esp32")
		startDone <- startErr
	}()
	go func() {
		<-start
		deleteDone <- manager.Delete("same")
	}()
	close(start)
	startErr, deleteErr := <-startDone, <-deleteDone
	if startErr == nil {
		if _, _, _, err := manager.Stop(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("started recording must survive a concurrent delete, info=%v err=%v", info, err)
		}
	} else {
		if deleteErr != nil {
			t.Fatalf("delete error=%v after start observed the existing completed file (start=%v)", deleteErr, startErr)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed recording should be deleted when deletion serialized first, err=%v", err)
		}
	}
}
