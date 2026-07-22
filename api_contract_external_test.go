package recorder_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	recorder "github.com/mgurevin/recorder"
)

func TestPublishedRecorderExtensionSchemaIsValidJSON(t *testing.T) {
	data, err := os.ReadFile("schema/recorder-har-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}

	if schema["$id"] == nil {
		t.Fatal("published schema has no stable $id")
	}
}

func TestConfigSurface(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  any
		fields []string
	}{
		{"Config", recorder.Config{}, []string{"CaptureRequestBody", "CaptureResponseBody", "Redaction", "BodyCapturePolicy", "HeadSamplingPolicy", "RetentionPolicy"}},
		{"AsyncRecorderConfig", recorder.AsyncRecorderConfig{}, []string{"QueueCapacity", "Backpressure", "BatchSize", "BlockTimeout", "InternalErrorMode", "OnInternalError", "Logf"}},
		{"FileBodyStoreConfig", recorder.FileBodyStoreConfig{}, []string{"MaxBytes", "MaxFiles", "PartialTTL", "SyncOnCommit"}},
	} {
		typeOf := reflect.TypeOf(test.value)
		for _, field := range test.fields {
			if _, ok := typeOf.FieldByName(field); !ok {
				t.Errorf("%s is missing frozen field %s", test.name, field)
			}
		}
	}
}

func TestRecorderExtensionWireContract(t *testing.T) {
	data, err := json.Marshal(&recorder.Entry{
		Recorder: &recorder.RecorderEntryExtension{
			SchemaVersion: recorder.RecorderExtensionVersion,
			TraceID:       "trace",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	text := string(data)
	if !strings.Contains(text, `"_recorder":{"schemaVersion":"1","traceId":"trace"}`) {
		t.Fatalf("unexpected extension encoding: %s", text)
	}

	for _, legacy := range []string{"_traceId", "_exchangeId", "_network", "_tls", "_redaction"} {
		if strings.Contains(text, legacy) {
			t.Errorf("legacy extension %q leaked into wire schema", legacy)
		}
	}
}
