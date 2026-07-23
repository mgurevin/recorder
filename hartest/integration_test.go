package hartest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
	"github.com/mgurevin/recorder/hartest"
)

func TestHARAndNDJSONProduceEquivalentFixtures(t *testing.T) {
	t.Parallel()

	captured := entry("POST", "https://api.example.com/orders?mode=test", `{"id":42}`, `{"ok":true}`)

	harBytes, err := json.Marshal(&recorder.HAR{
		Log: &recorder.Log{
			Version: "1.2",
			Creator: &recorder.Creator{
				Name:    "hartest integration test",
				Version: "1",
			},
			Entries: []*recorder.Entry{captured},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ndjsonBytes, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}

	ndjsonBytes = append(ndjsonBytes, '\n')

	document, err := hario.ReadHAR(bytes.NewReader(harBytes), hario.DefaultReadConfig())
	if err != nil {
		t.Fatal(err)
	}

	ndjsonEntries, err := hario.ReadNDJSON(bytes.NewReader(ndjsonBytes), hario.DefaultReadConfig())
	if err != nil {
		t.Fatal(err)
	}

	for name, entries := range map[string][]*recorder.Entry{
		"har":    document.Log.Entries,
		"ndjson": ndjsonEntries,
	} {
		t.Run(name, func(t *testing.T) {
			fixture, err := hartest.NewTransport(entries, hartest.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}

			request, err := http.NewRequest(
				http.MethodPost,
				"https://api.example.com/orders?mode=test",
				bytes.NewBufferString(`{"id":42}`),
			)
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Content-Type", "application/json")

			response, err := (&http.Client{Transport: fixture}).Do(request)
			if err != nil {
				t.Fatal(err)
			}

			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()

			if readErr != nil {
				t.Fatal(readErr)
			}

			if closeErr != nil {
				t.Fatal(closeErr)
			}

			if string(body) != `{"ok":true}` {
				t.Fatalf("body = %q", body)
			}

			if err := fixture.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
