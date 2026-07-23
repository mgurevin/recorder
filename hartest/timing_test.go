package hartest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/mgurevin/recorder"
)

func TestReplayTimingConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		timing ReplayTimingConfig
		valid  bool
	}{
		{name: "disabled", timing: ReplayTimingConfig{}, valid: true},
		{name: "enabled", timing: ReplayTimingConfig{Scale: 1, MaxDelay: time.Second}, valid: true},
		{name: "negative scale", timing: ReplayTimingConfig{Scale: -1, MaxDelay: time.Second}},
		{name: "NaN scale", timing: ReplayTimingConfig{Scale: math.NaN(), MaxDelay: time.Second}},
		{name: "infinite scale", timing: ReplayTimingConfig{Scale: math.Inf(1), MaxDelay: time.Second}},
		{name: "enabled without bound", timing: ReplayTimingConfig{Scale: 1}},
		{name: "bound while disabled", timing: ReplayTimingConfig{MaxDelay: time.Second}},
		{name: "negative bound", timing: ReplayTimingConfig{Scale: 1, MaxDelay: -time.Second}},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			config := DefaultConfig()
			config.Timing = test.timing

			err := validateConfig(config)
			if test.valid && err != nil {
				t.Fatalf("validateConfig() = %v", err)
			}

			if !test.valid && err == nil {
				t.Fatal("validateConfig() = nil")
			}
		})
	}
}

func TestReplayTimingPlanScalesAndBoundsRecordedDurations(t *testing.T) {
	t.Parallel()

	entry := &recorder.Entry{Timings: &recorder.Timings{
		Blocked: 10,
		DNS:     20,
		Connect: 30,
		SSL:     900, // SSL is already represented inside connect.
		Send:    40,
		Wait:    50,
		Receive: 60,
	}}

	timing := replayTimingPlan(entry, ReplayTimingConfig{
		Scale:    0.5,
		MaxDelay: 100 * time.Millisecond,
	})

	if timing.beforeResponse != 75*time.Millisecond {
		t.Errorf("beforeResponse = %s", timing.beforeResponse)
	}

	if timing.body != 25*time.Millisecond {
		t.Errorf("body = %s", timing.body)
	}
}

func TestReplayTimingPlanIgnoresUnavailableDurations(t *testing.T) {
	t.Parallel()

	entry := &recorder.Entry{Timings: &recorder.Timings{
		Blocked: -1,
		DNS:     math.NaN(),
		Connect: math.Inf(1),
		Send:    2,
		Wait:    3,
		Receive: 4,
	}}

	timing := replayTimingPlan(entry, ReplayTimingConfig{
		Scale:    2,
		MaxDelay: time.Second,
	})

	if timing.beforeResponse != 10*time.Millisecond {
		t.Errorf("beforeResponse = %s", timing.beforeResponse)
	}

	if timing.body != 8*time.Millisecond {
		t.Errorf("body = %s", timing.body)
	}
}

func TestFixtureBodyDistributesReceiveDelayAcrossReads(t *testing.T) {
	t.Parallel()

	var delays []time.Duration

	body := &fixtureBody{
		reader:  bytes.NewReader([]byte("abcd")),
		bytes:   []byte("abcd"),
		context: context.Background(),
		delay:   40 * time.Millisecond,
		wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)

			return nil
		},
	}

	first := make([]byte, 1)
	if n, err := body.Read(first); n != 1 || err != nil || string(first) != "a" {
		t.Fatalf("first Read() = %d, %v, %q", n, err, first)
	}

	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}

	if string(rest) != "bcd" {
		t.Fatalf("remaining body = %q", rest)
	}

	if len(delays) != 2 || delays[0] != 10*time.Millisecond || delays[1] != 30*time.Millisecond {
		t.Fatalf("delays = %v", delays)
	}
}

func TestTransportAppliesTimingWithoutSleeping(t *testing.T) {
	t.Parallel()

	entry := timingFixtureEntry()
	config := DefaultConfig()
	config.Match.Headers = nil
	config.Timing = ReplayTimingConfig{Scale: 1, MaxDelay: time.Second}

	fixture, err := NewTransport([]*recorder.Entry{entry}, config)
	if err != nil {
		t.Fatal(err)
	}

	var delays []time.Duration

	fixture.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)

		return nil
	}

	request, err := http.NewRequest(http.MethodGet, entry.Request.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := fixture.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "body" {
		t.Fatalf("body = %q", body)
	}

	if len(delays) != 2 || delays[0] != 5*time.Millisecond || delays[1] != 4*time.Millisecond {
		t.Fatalf("delays = %v", delays)
	}

	if err := fixture.Verify(); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
}

func TestTransportAppliesTimingToRecordedFailure(t *testing.T) {
	t.Parallel()

	entry := timingFixtureEntry()
	entry.Response.Status = 0
	entry.Response.StatusText = ""
	entry.Recorder.Error = &recorder.ErrorInfo{
		Phase:   recorder.PhaseWaitResponse,
		Message: "recorded timeout",
		Timeout: true,
	}

	config := DefaultConfig()
	config.Match.Headers = nil
	config.Timing = ReplayTimingConfig{Scale: 1, MaxDelay: time.Second}

	fixture, err := NewTransport([]*recorder.Entry{entry}, config)
	if err != nil {
		t.Fatal(err)
	}

	var delays []time.Duration

	fixture.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)

		return nil
	}

	request, err := http.NewRequest(http.MethodGet, entry.Request.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.RoundTrip(request)

	var recorded *RecordedError
	if !errors.As(err, &recorded) || !recorded.Timeout() {
		t.Fatalf("RoundTrip() error = %#v", err)
	}

	if len(delays) != 1 || delays[0] != 5*time.Millisecond {
		t.Fatalf("delays = %v", delays)
	}

	if err := fixture.Verify(); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
}

func TestWaitContextStopsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitContext() = %v", err)
	}
}

func timingFixtureEntry() *recorder.Entry {
	body := "body"

	return &recorder.Entry{
		StartedDateTime: "2026-07-23T00:00:00.000Z",
		Time:            9,
		Request: &recorder.Request{
			Method:      http.MethodGet,
			URL:         "https://api.example.test/timed",
			HTTPVersion: "HTTP/1.1",
			HeadersSize: -1,
			BodySize:    0,
		},
		Response: &recorder.Response{
			Status:      http.StatusOK,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Content:     &recorder.Content{Size: int64(len(body)), MimeType: "text/plain", Text: body},
			HeadersSize: -1,
			BodySize:    int64(len(body)),
		},
		Cache: &recorder.Cache{},
		Timings: &recorder.Timings{
			Blocked: -1,
			DNS:     -1,
			Connect: -1,
			Send:    2,
			Wait:    3,
			Receive: 4,
			SSL:     -1,
		},
		Recorder: &recorder.RecorderEntryExtension{
			SchemaVersion: recorder.RecorderExtensionVersion,
			RequestBody:   &recorder.BodyInfo{Complete: true},
			ResponseBody: &recorder.BodyInfo{
				Present:       true,
				Complete:      true,
				CapturedBytes: int64(len(body)),
				TotalBytes:    int64(len(body)),
			},
		},
	}
}
