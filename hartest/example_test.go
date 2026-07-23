package hartest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hartest"
)

func ExampleNewTransport() {
	captured := entry(
		http.MethodPost,
		"https://api.example.com/orders",
		`{"amount":42}`,
		`{"status":"approved"}`,
	)

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, hartest.DefaultConfig())
	if err != nil {
		panic(err)
	}

	client := &http.Client{Transport: fixture}

	request, err := http.NewRequest(
		http.MethodPost,
		captured.Request.URL,
		strings.NewReader(`{"amount":42}`),
	)
	if err != nil {
		panic(err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		panic(err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}

	if err := response.Body.Close(); err != nil {
		panic(err)
	}

	if err := fixture.Verify(); err != nil {
		panic(err)
	}

	fmt.Println(string(body))
	// Output: {"status":"approved"}
}

func ExampleRequestNormalizer() {
	captured := entry(
		http.MethodPost,
		"https://api.example.com/orders/recorded?timestamp=recorded",
		`{"amount":42,"requestId":"recorded"}`,
		`{"status":"approved"}`,
	)
	config := hartest.DefaultConfig()
	config.Match.Normalize = func(request *hartest.RequestSnapshot) error {
		request.URL.Path = "/orders/{id}"

		query := request.URL.Query()
		query.Del("timestamp")
		request.URL.RawQuery = query.Encode()

		var body map[string]any
		if err := json.Unmarshal(request.Body, &body); err != nil {
			return err
		}

		delete(body, "requestId")

		normalized, err := json.Marshal(body)
		if err != nil {
			return err
		}

		request.Body = normalized

		return nil
	}

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		panic(err)
	}

	request, err := http.NewRequest(
		http.MethodPost,
		"https://api.example.com/orders/live?timestamp=live",
		strings.NewReader(`{"requestId":"live","amount":42}`),
	)
	if err != nil {
		panic(err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := (&http.Client{Transport: fixture}).Do(request)
	if err != nil {
		panic(err)
	}

	if err := response.Body.Close(); err != nil {
		panic(err)
	}

	fmt.Println(response.StatusCode)
	// Output: 200
}
