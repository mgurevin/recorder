package recorder

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDebugStreamRecorderPublishesSSEToSingleLocalSubscriber(t *testing.T) {
	recorder, err := NewDebugStreamRecorder(DefaultDebugStreamRecorderConfig())
	if err != nil {
		t.Fatalf("NewDebugStreamRecorder: %v", err)
	}

	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	request.Header.Set("Origin", "http://localhost:5173")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	t.Cleanup(func() { _ = response.Body.Close() })

	if response.StatusCode != http.StatusOK {
		t.Fatalf("subscribe status = %d", response.StatusCode)
	}

	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("allow origin = %q", got)
	}

	second, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}

	if second.StatusCode != http.StatusConflict {
		t.Errorf("second subscribe status = %d, want %d", second.StatusCode, http.StatusConflict)
	}

	_ = second.Body.Close()

	entry := &Entry{StartedDateTime: "2026-07-22T00:00:00.000Z", Request: &Request{Method: http.MethodGet}}

	if err := recorder.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	scanner := bufio.NewScanner(response.Body)
	foundEntry := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: {") && strings.Contains(line, `"startedDateTime":"2026-07-22`) {
			foundEntry = true

			break
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}

	if !foundEntry {
		t.Fatal("entry event not found")
	}

	stats := recorder.Stats()
	if !stats.SubscriberActive || stats.Published != 1 || stats.Dropped != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestDebugStreamRecorderDropsOldestAndReportsGap(t *testing.T) {
	recorder, err := NewDebugStreamRecorder(DebugStreamRecorderConfig{QueueCapacity: 2})
	if err != nil {
		t.Fatalf("NewDebugStreamRecorder: %v", err)
	}

	queue, ok := recorder.subscribe()
	if !ok {
		t.Fatal("subscribe rejected")
	}

	for i := 0; i < 3; i++ {
		if err := recorder.Record(&Entry{Time: float64(i)}); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	first := <-queue
	second := <-queue

	if first.id != 2 || second.id != 3 {
		t.Fatalf("queued ids = %d, %d; want 2, 3", first.id, second.id)
	}

	if got := recorder.takePendingDrops(queue); got != 1 {
		t.Fatalf("pending drops = %d, want 1", got)
	}

	stats := recorder.Stats()
	if stats.Published != 3 || stats.Dropped != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestDebugStreamRecorderReplaysDisconnectedBacklog(t *testing.T) {
	recorder, err := NewDebugStreamRecorder(DebugStreamRecorderConfig{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("NewDebugStreamRecorder: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if err := recorder.Record(&Entry{Time: float64(i)}); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	queue, ok := recorder.subscribe()
	if !ok {
		t.Fatal("subscribe rejected")
	}

	first := <-queue
	second := <-queue

	if first.id != 1 || second.id != 2 {
		t.Fatalf("replayed ids = %d, %d; want 1, 2", first.id, second.id)
	}
}

func TestDebugStreamRecorderRejectsNonLoopbackAndClose(t *testing.T) {
	recorder, err := NewDebugStreamRecorder(DefaultDebugStreamRecorderConfig())
	if err != nil {
		t.Fatalf("NewDebugStreamRecorder: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.test/entries", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	recorder.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Errorf("non-loopback status = %d, want %d", response.Code, http.StatusForbidden)
	}

	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := recorder.Record(&Entry{}); !errors.Is(err, ErrDebugStreamRecorderClosed) {
		t.Fatalf("Record after Close error = %v", err)
	}
}

func TestNewDebugStreamRecorderValidatesCapacity(t *testing.T) {
	if _, err := NewDebugStreamRecorder(DebugStreamRecorderConfig{}); err == nil {
		t.Fatal("zero queue capacity accepted")
	}
}
