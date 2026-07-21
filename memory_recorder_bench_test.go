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

	for range b.N {
		recorder.Record(entry)
	}
}

func BenchmarkMemoryRecorderEntriesWrapped(b *testing.B) {
	recorder, err := NewMemoryRecorderWithCapacity(1024)
	if err != nil {
		b.Fatal(err)
	}

	entry := &Entry{}
	for range 2048 {
		recorder.Record(entry)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = recorder.Entries()
	}
}
