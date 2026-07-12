package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessmar/lambda-pic-scaler/scaler"
)

type obj struct {
	body []byte
	meta map[string]string
}

// memStore is an in-memory bucket that, like S3 + EventBridge, emits an
// "Object Created" event for EVERY put. That is what makes feedback loops observable.
type memStore struct {
	objs    map[string]obj
	events  []string // keys of pending events
	puts    int
	gets    int
	dropAll bool // simulate a bucket that loses all user metadata
}

func newStore() *memStore { return &memStore{objs: map[string]obj{}} }

func (m *memStore) Head(_ context.Context, _, key string) (map[string]string, int64, error) {
	o, ok := m.objs[key]
	if !ok {
		return nil, 0, errors.New("not found")
	}
	return maps.Clone(o.meta), int64(len(o.body)), nil
}

func (m *memStore) Get(_ context.Context, _, key string) ([]byte, error) {
	m.gets++
	return m.objs[key].body, nil
}

func (m *memStore) Put(_ context.Context, _, key string, body []byte, _ string, meta map[string]string) error {
	m.puts++
	if m.dropAll {
		meta = nil
	}
	m.objs[key] = obj{body, maps.Clone(meta)}
	m.events = append(m.events, key)
	return nil
}

// upload is a user upload (also emits an event).
func (m *memStore) upload(key string, meta map[string]string) {
	m.objs[key] = obj{[]byte("img"), meta}
	m.events = append(m.events, key)
}

func fakeResize(_ []byte, s scaler.Spec) ([]byte, string, error) {
	return fmt.Appendf(nil, "%dx%d", s.W, s.H), "image/jpeg", nil
}

func newHandler(m *memStore) *Handler {
	return &Handler{Store: m, Resize: fakeResize, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func event(key string) events.CloudWatchEvent {
	d, _ := json.Marshal(map[string]any{
		"bucket": map[string]string{"name": "b"}, "object": map[string]string{"key": key},
	})
	return events.CloudWatchEvent{Detail: d}
}

// drain delivers events until none are left. It fails the test if the number of
// invocations exceeds limit: that is a feedback loop.
func drain(t *testing.T, h *Handler, m *memStore, limit int) (invocations int) {
	t.Helper()
	for len(m.events) > 0 {
		if invocations++; invocations > limit {
			t.Fatalf("FEEDBACK LOOP: more than %d invocations, %d puts", limit, m.puts)
		}
		key := m.events[0]
		m.events = m.events[1:]
		if err := h.Handle(t.Context(), event(key)); err != nil {
			t.Fatal(err)
		}
	}
	return invocations
}

const threeSpecs = `[{"w":800,"postfix":"-a"},{"w":400,"postfix":"-b"},{"w":100,"postfix":"-c"}]`

// One upload must cost exactly 1 + N invocations (the upload, then each output skipped) and N puts.
func TestNoFeedbackLoop(t *testing.T) {
	m := newStore()
	m.upload("p/x.jpg", map[string]string{"resize": threeSpecs})
	n := drain(t, newHandler(m), m, 1+3)
	if n != 4 || m.puts != 3 {
		t.Fatalf("invocations=%d puts=%d, want 4 and 3", n, m.puts)
	}
}

// Even a chain of 100 uploads stays linear: no output ever re-triggers work.
func TestWorkIsLinearInUploads(t *testing.T) {
	m := newStore()
	for i := range 100 {
		m.upload(fmt.Sprintf("p/%d.jpg", i), map[string]string{"resize": threeSpecs})
	}
	n := drain(t, newHandler(m), m, 100*4)
	if n != 400 || m.puts != 300 {
		t.Fatalf("invocations=%d puts=%d", n, m.puts)
	}
}

func TestOutputsCarryMarkerAndNeverInstructions(t *testing.T) {
	m := newStore()
	m.upload("x.jpg", map[string]string{"resize": threeSpecs, "other": "keep-out"})
	drain(t, newHandler(m), m, 10)
	for k, o := range m.objs {
		if k == "x.jpg" {
			continue
		}
		if o.meta[scaler.OutputMarkerKey] != scaler.OutputMarkerValue {
			t.Errorf("%s: missing marker: %v", k, o.meta)
		}
		if _, has := o.meta[scaler.MetadataKey]; has || len(o.meta) != 1 {
			t.Errorf("%s: metadata must be only the marker: %v", k, o.meta)
		}
	}
}

// Guard 1 alone: an object with the marker is skipped even though it carries valid instructions
// (e.g. a copy of an output with metadata added by hand). It must not even be downloaded.
func TestMarkedObjectIsSkippedEvenWithInstructions(t *testing.T) {
	for _, markerKey := range []string{"resized-by", "Resized-By", "RESIZED-BY"} {
		m := newStore()
		m.upload("o.jpg", map[string]string{"resize": threeSpecs, markerKey: "anything"})
		drain(t, newHandler(m), m, 1)
		if m.puts != 0 || m.gets != 0 {
			t.Errorf("%s: puts=%d gets=%d, want 0", markerKey, m.puts, m.gets)
		}
	}
}

// Guard 2 alone: if metadata is lost entirely (marker included) outputs still stop, for lack of instructions.
func TestLosingAllMetadataStillStopsLoop(t *testing.T) {
	m := newStore()
	m.dropAll = true
	m.upload("x.jpg", map[string]string{"resize": threeSpecs})
	m.objs["x.jpg"] = obj{[]byte("img"), map[string]string{"resize": threeSpecs}} // input keeps its metadata
	if n := drain(t, newHandler(m), m, 4); n != 4 || m.puts != 3 {
		t.Fatalf("invocations=%d puts=%d", n, m.puts)
	}
}

func TestNoInstructionsMeansNoWork(t *testing.T) {
	m := newStore()
	m.upload("plain.jpg", nil)
	drain(t, newHandler(m), m, 1)
	if m.puts != 0 || m.gets != 0 {
		t.Fatalf("puts=%d gets=%d", m.puts, m.gets)
	}
}

// Bad input never produces writes, and is not retried (nil error) except real failures.
func TestBadInputWritesNothing(t *testing.T) {
	for name, meta := range map[string]string{
		"not json":      `nope`,
		"empty postfix": `[{"w":10,"postfix":""}]`,
		"too many":      "[" + strings.Repeat(`{"w":1,"postfix":"-x"},`, 11) + `{"w":1,"postfix":"-y"}]`,
	} {
		m := newStore()
		m.upload("x.jpg", map[string]string{"resize": meta})
		drain(t, newHandler(m), m, 1)
		if m.puts != 0 {
			t.Errorf("%s: wrote %d objects", name, m.puts)
		}
	}
}

func TestUnsupportedImageWritesNothing(t *testing.T) {
	m := newStore()
	m.upload("x.txt", map[string]string{"resize": threeSpecs})
	h := newHandler(m)
	h.Resize = func([]byte, scaler.Spec) ([]byte, string, error) { return nil, "", scaler.ErrUnsupported }
	drain(t, h, m, 1)
	if m.puts != 0 {
		t.Fatalf("puts=%d", m.puts)
	}
}

// A failing second spec must not leave a half-written set behind (retries would otherwise multiply writes).
func TestResizeFailureWritesNothingAndReturnsError(t *testing.T) {
	m := newStore()
	m.upload("x.jpg", map[string]string{"resize": threeSpecs})
	h := newHandler(m)
	calls := 0
	h.Resize = func(b []byte, s scaler.Spec) ([]byte, string, error) {
		if calls++; calls == 2 {
			return nil, "", errors.New("boom")
		}
		return fakeResize(b, s)
	}
	if err := h.Handle(t.Context(), event("x.jpg")); err == nil {
		t.Fatal("expected error so Lambda retries")
	}
	if m.puts != 0 {
		t.Fatalf("puts=%d, want 0", m.puts)
	}
}

func TestOversizedInputSkipped(t *testing.T) {
	m := newStore()
	m.objs["big.jpg"] = obj{make([]byte, MaxInputBytes+1), map[string]string{"resize": threeSpecs}}
	m.events = []string{"big.jpg"}
	drain(t, newHandler(m), m, 1)
	if m.puts != 0 || m.gets != 0 {
		t.Fatalf("puts=%d gets=%d", m.puts, m.gets)
	}
}

// Re-delivering the same event (at-least-once) rewrites the same keys: no growth.
func TestRedeliveryIsIdempotent(t *testing.T) {
	m := newStore()
	m.upload("x.jpg", map[string]string{"resize": threeSpecs})
	h := newHandler(m)
	for range 3 {
		if err := h.Handle(t.Context(), event("x.jpg")); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.objs) != 4 {
		t.Fatalf("objects=%d, want 4", len(m.objs))
	}
}

func TestMalformedEventIsNotRetried(t *testing.T) {
	m := newStore()
	if err := newHandler(m).Handle(t.Context(), events.CloudWatchEvent{Detail: []byte(`[`)}); err != nil {
		t.Fatal(err)
	}
}
