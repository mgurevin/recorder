package recorder

import "testing"

func BenchmarkMemoryRecorderRecord(b *testing.B) {
	recorder, err := NewMemoryRecorderWithCapacity(1024)
	if err != nil {
		b.Fatal(err)
	}

	entry := &Entry{}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = recorder.Record(entry)
	}
}

func BenchmarkMemoryRecorderEntriesWrapped(b *testing.B) {
	recorder, err := NewMemoryRecorderWithCapacity(1024)
	if err != nil {
		b.Fatal(err)
	}

	entry := &Entry{}
	for range 2048 {
		_ = recorder.Record(entry)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = recorder.Entries()
	}
}
