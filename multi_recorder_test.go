package recorder_test

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	recorder "github.com/mgurevin/recorder"
)

func TestMultiRecorderDeliversSameEntryInOrder(t *testing.T) {
	var calls []string

	entry := &recorder.Entry{Comment: "same pointer"}

	first := recorder.RecorderFunc(func(got *recorder.Entry) error {
		if got != entry {
			t.Error("first recorder received a cloned entry")
		}

		calls = append(calls, "first")

		return nil
	})
	second := recorder.RecorderFunc(func(got *recorder.Entry) error {
		if got != entry {
			t.Error("second recorder received a cloned entry")
		}

		calls = append(calls, "second")

		return nil
	})

	multi := recorder.NewMultiRecorder(first, second)

	if err := multi.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestMultiRecorderJoinsFailuresWithoutShortCircuiting(t *testing.T) {
	firstError := errors.New("first failed")
	secondError := errors.New("second failed")
	called := 0

	multi := recorder.NewMultiRecorder(
		recorder.RecorderFunc(func(*recorder.Entry) error {
			called++

			return firstError
		}),
		recorder.RecorderFunc(func(*recorder.Entry) error {
			called++

			return secondError
		}),
	)

	err := multi.Record(&recorder.Entry{})

	if called != 2 {
		t.Fatalf("called = %d, want 2", called)
	}

	if !errors.Is(err, firstError) || !errors.Is(err, secondError) {
		t.Fatalf("Record error = %v", err)
	}

	if message := err.Error(); !strings.Contains(message, "multi recorder 0") || !strings.Contains(message, "multi recorder 1") {
		t.Errorf("Record error lacks sink indexes: %v", err)
	}
}

func TestMultiRecorderPreservesBatchCapability(t *testing.T) {
	batch := &batchSpy{}
	ordinary := &recordSpy{}

	multi := recorder.NewMultiRecorder(batch, ordinary)

	batchCapable, ok := multi.(interface {
		RecordBatch([]*recorder.Entry) error
	})
	if !ok {
		t.Fatal("multi recorder does not expose batch capability")
	}

	entries := []*recorder.Entry{{Comment: "a"}, {Comment: "b"}}

	if err := batchCapable.RecordBatch(entries); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}

	if batch.batchCalls != 1 || batch.recordCalls != 0 || !reflect.DeepEqual(batch.entries, entries) {
		t.Errorf("batch recorder = %+v", batch)
	}

	if ordinary.recordCalls != 2 || !reflect.DeepEqual(ordinary.entries, entries) {
		t.Errorf("ordinary recorder = %+v", ordinary)
	}
}

func TestMultiRecorderBatchJoinsFailuresAndContinuesFallback(t *testing.T) {
	batchError := errors.New("batch failed")
	recordError := errors.New("record failed")
	batch := &batchSpy{batchError: batchError}
	ordinary := &recordSpy{recordError: recordError}
	multi := recorder.NewMultiRecorder(batch, ordinary)
	batchCapable := multi.(interface {
		RecordBatch([]*recorder.Entry) error
	})

	err := batchCapable.RecordBatch([]*recorder.Entry{{}, {}})

	if !errors.Is(err, batchError) || !errors.Is(err, recordError) {
		t.Fatalf("RecordBatch error = %v", err)
	}

	if batch.batchCalls != 1 || ordinary.recordCalls != 2 {
		t.Errorf("call counts = batch %d, fallback %d", batch.batchCalls, ordinary.recordCalls)
	}
}

func TestNewMultiRecorderRejectsNilAndCollapsesSingleInput(t *testing.T) {
	t.Run("nil first", func(t *testing.T) {
		assertPanics(t, func() { recorder.NewMultiRecorder(nil) })
	})
	t.Run("nil additional", func(t *testing.T) {
		assertPanics(t, func() {
			recorder.NewMultiRecorder(recorder.RecorderFunc(func(*recorder.Entry) error { return nil }), nil)
		})
	})

	single := recorder.RecorderFunc(func(*recorder.Entry) error { return nil })

	multi := recorder.NewMultiRecorder(single)

	if reflect.ValueOf(multi).Pointer() != reflect.ValueOf(single).Pointer() {
		t.Fatal("single recorder was wrapped")
	}
}

func TestMultiRecorderConcurrentUse(t *testing.T) {
	first := &recordSpy{}
	second := &recordSpy{}

	multi := recorder.NewMultiRecorder(first, second)

	const count = 100

	var wait sync.WaitGroup
	wait.Add(count)

	for range count {
		go func() {
			defer wait.Done()

			if err := multi.Record(&recorder.Entry{}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
	}

	wait.Wait()

	if first.count() != count || second.count() != count {
		t.Errorf("record counts = %d, %d", first.count(), second.count())
	}
}

func assertPanics(t *testing.T, call func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Error("call did not panic")
		}
	}()

	call()
}

type recordSpy struct {
	mu          sync.Mutex
	recordCalls int
	entries     []*recorder.Entry
	recordError error
}

func (s *recordSpy) Record(entry *recorder.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recordCalls++
	s.entries = append(s.entries, entry)

	return s.recordError
}

func (s *recordSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.recordCalls
}

type batchSpy struct {
	recordSpy
	batchCalls int
	batchError error
}

func (s *batchSpy) RecordBatch(entries []*recorder.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.batchCalls++
	s.entries = append(s.entries, entries...)

	return s.batchError
}
