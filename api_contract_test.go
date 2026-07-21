package recorder

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPublicAPIFreezeRules(t *testing.T) {
	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range packages["recorder"].Files {
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				contextHelpers := map[string]bool{"WithTraceID": true, "WithSamplingKey": true, "WithRequestRedaction": true}
				if value.Recv == nil && ast.IsExported(value.Name.Name) && strings.HasPrefix(value.Name.Name, "With") && !contextHelpers[value.Name.Name] {
					t.Errorf("functional option %s must be represented by a Config field", value.Name.Name)
				}

			case *ast.GenDecl:
				for _, spec := range value.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !ast.IsExported(typeSpec.Name.Name) {
						continue
					}

					name := typeSpec.Name.Name
					if strings.HasSuffix(name, "Option") || strings.HasSuffix(name, "PolicyFunc") || name == "BatchRecorder" || name == "EntryAssetReleaser" {
						t.Errorf("maintenance-only type %s is exported", name)
					}
				}
			}
		}
	}
}

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
		{"Config", Config{}, []string{"CaptureRequestBody", "CaptureResponseBody", "Redaction", "BodyCapturePolicy", "HeadSamplingPolicy", "RetentionPolicy"}},
		{"AsyncRecorderConfig", AsyncRecorderConfig{}, []string{"QueueCapacity", "Backpressure", "BatchSize", "BlockTimeout"}},
		{"FileBodyStoreConfig", FileBodyStoreConfig{}, []string{"MaxBytes", "MaxFiles", "PartialTTL", "SyncOnCommit"}},
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
	data, err := json.Marshal(&Entry{Recorder: &RecorderEntryExtension{SchemaVersion: RecorderExtensionVersion, TraceID: "trace"}})
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
