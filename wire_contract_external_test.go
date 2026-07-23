package recorder_test

import (
	"encoding/json"
	"os"
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

	const schemaID = "https://github.com/mgurevin/recorder/schema/recorder-har-v1.schema.json"

	if actual := schema["$id"]; actual != schemaID {
		t.Fatalf("schema $id = %v, want %q", actual, schemaID)
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema properties is not an object")
	}

	version, ok := properties["schemaVersion"].(map[string]any)
	if !ok {
		t.Fatal("schemaVersion property is not an object")
	}

	if actual := version["const"]; actual != recorder.RecorderExtensionVersion {
		t.Fatalf(
			"schemaVersion const = %v, want RecorderExtensionVersion %q",
			actual,
			recorder.RecorderExtensionVersion,
		)
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
