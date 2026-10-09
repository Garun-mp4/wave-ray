package recording

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruvnet/ruview/backend/internal/security"
	"github.com/ruvnet/ruview/backend/internal/ws"
)

const (
	maxFrameBytes    = 2 << 20
	flushEveryFrames = 100
)

var safeRecordingID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var (
	ErrRecordingActive   = errors.New("a recording is already active")
	ErrNoActiveRecording = errors.New("no recording is active")
	ErrInvalidID         = errors.New("invalid recording id")
	ErrRecordingNotFound = errors.New("recording was not found")
	ErrActiveDelete      = errors.New("cannot delete an active recording")
)

type Subscription interface {
	Subscribe() *ws.Subscription
}

type Manager struct {
	root        string
	hub         Subscription
	privacyMode bool
	mu          sync.Mutex
	active      *activeRecording
}

type activeRecording struct {
	id        string
	started   time.Time
	file      *os.File
	sub       *ws.Subscription
	cancel    context.CancelFunc
	done      chan struct{}
	frames    atomic.Uint64
	writeErr  error
	watermark *string
}

type Info struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Path          string  `json:"path"`
	SizeBytes     int64   `json:"size_bytes"`
	Frames        uint64  `json:"frames"`
	ModifiedEpoch int64   `json:"modified_epoch"`
	Status        string  `json:"status"`
	DurationSecs  *int64  `json:"duration_secs,omitempty"`
	Watermark     *string `json:"watermark,omitempty"`
}

func New(dataDir string, hub Subscription, privacyMode bool) (*Manager, error) {
	if strings.TrimSpace(dataDir) == "" || hub == nil {
		return nil, errors.New("recording directory and stream hub are required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "recordings"))
	if err != nil {
		return nil, fmt.Errorf("resolve recording directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create recording directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recording directory must be a real directory")
	}
	return &Manager{root: root, hub: hub, privacyMode: privacyMode}, nil
}

func (m *Manager) Start(id, source string) (string, *string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return m.active.id, clone(m.active.watermark), ErrRecordingActive
	}
	if id == "" {
		id = fmt.Sprintf("rec_%s", time.Now().UTC().Format("20060102_150405.000000000"))
	}
	if !validID(id) {
		return "", nil, ErrInvalidID
	}
	path := filepath.Join(m.root, id+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("create recording: %w", err)
	}
	var watermark *string
	if source == "simulate" || source == "synthetic" {
		mark := "SYNTHETIC"
		watermark = &mark
	}
	ctx, cancel := context.WithCancel(context.Background())
	active := &activeRecording{
		id: id, started: time.Now(), file: file, sub: m.hub.Subscribe(),
		cancel: cancel, done: make(chan struct{}), watermark: watermark,
	}
	m.active = active
	go m.writeLoop(ctx, active)
	return id, clone(watermark), nil
}

func (m *Manager) Stop() (id string, frames uint64, duration time.Duration, err error) {
	m.mu.Lock()
	active := m.active
	if active == nil {
		m.mu.Unlock()
		return "", 0, 0, ErrNoActiveRecording
	}
	active.cancel()
	m.mu.Unlock()
	<-active.done
	m.mu.Lock()
	if m.active == active {
		m.active = nil
	}
	m.mu.Unlock()
	return active.id, active.frames.Load(), time.Since(active.started), active.writeErr
}

func (m *Manager) Active() (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return Info{}, false
	}
	active := m.active
	duration := int64(time.Since(active.started).Seconds())
	return Info{
		ID: active.id, Name: active.id, Path: filepath.Join("recordings", active.id+".jsonl"),
		Frames: active.frames.Load(), Status: "recording", DurationSecs: &duration, Watermark: clone(active.watermark),
	}, true
}

func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("scan recordings directory: %w", err)
	}
	active, isActive := m.Active()
	items := make([]Info, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".jsonl") || !entry.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !validID(id) {
			continue
		}
		path := filepath.Join(m.root, name)
		fileInfo, statErr := os.Lstat(path)
		if statErr != nil || !fileInfo.Mode().IsRegular() {
			continue
		}
		item := Info{
			ID: id, Name: id, Path: filepath.Join("recordings", name), SizeBytes: fileInfo.Size(),
			ModifiedEpoch: fileInfo.ModTime().Unix(), Status: "completed",
		}
		var scanErr error
		item.Frames, item.Watermark, scanErr = scanFrames(path)
		if scanErr != nil {
			item.Status = "invalid"
		}
		if isActive && active.ID == id {
			item.Status, item.Frames, item.DurationSecs, item.Watermark = active.Status, active.Frames, active.DurationSecs, clone(active.Watermark)
		}
		items = append(items, item)
	}
	return items, nil
}

func (m *Manager) Delete(id string) error {
	if !validID(id) {
		return ErrInvalidID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil && m.active.id == id {
		return ErrActiveDelete
	}
	path := filepath.Join(m.root, id+".jsonl")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrRecordingNotFound
	}
	if err != nil {
		return fmt.Errorf("inspect recording: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("refusing to delete non-regular recording file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete recording: %w", err)
	}
	return nil
}

func (m *Manager) writeLoop(ctx context.Context, active *activeRecording) {
	defer close(active.done)
	defer active.sub.Close()
	writer := bufio.NewWriterSize(active.file, 64<<10)
	defer func() {
		if err := writer.Flush(); err != nil && active.writeErr == nil {
			active.writeErr = err
		}
		if err := active.file.Close(); err != nil && active.writeErr == nil {
			active.writeErr = err
		}
		if active.writeErr != nil {
			m.mu.Lock()
			if m.active == active {
				m.active = nil
			}
			m.mu.Unlock()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case frame, ok := <-active.sub.C:
					if !ok {
						return
					}
					if !m.writeFrame(writer, active, frame) {
						return
					}
				default:
					return
				}
			}
		case frame, ok := <-active.sub.C:
			if !ok {
				active.writeErr = errors.New("recording stream ended because its bounded subscriber was closed")
				return
			}
			if !m.writeFrame(writer, active, frame) {
				return
			}
		}
	}
}

func (m *Manager) writeFrame(writer *bufio.Writer, active *activeRecording, frame []byte) bool {
	if len(frame) == 0 || len(frame) > maxFrameBytes {
		active.writeErr = errors.New("recording frame exceeds the supported size limit")
		return false
	}
	if m.privacyMode {
		filtered, err := security.RedactJSON(frame, maxFrameBytes)
		if err != nil {
			active.writeErr = errors.New("recording frame withheld by privacy filter")
			return false
		}
		frame = filtered
	}
	if _, err := writer.Write(frame); err != nil {
		active.writeErr = err
		return false
	}
	if err := writer.WriteByte('\n'); err != nil {
		active.writeErr = err
		return false
	}
	frames := active.frames.Add(1)
	if frames%flushEveryFrames == 0 {
		if err := writer.Flush(); err != nil {
			active.writeErr = err
			return false
		}
	}
	return true
}

func scanFrames(path string) (uint64, *string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes+1)
	var count uint64
	var watermark *string
	first := true
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		count++
		if first {
			first = false
			var source struct {
				Source string `json:"source"`
			}
			if json.Unmarshal(line, &source) == nil && (source.Source == "simulate" || source.Source == "synthetic") {
				mark := "SYNTHETIC"
				watermark = &mark
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return count, watermark, fmt.Errorf("recording contains a line above the supported size limit: %w", err)
	}
	return count, watermark, nil
}

func validID(id string) bool { return safeRecordingID.MatchString(id) && id != "." && id != ".." }

func clone(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
