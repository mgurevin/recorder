// Command har-fixture demonstrates an optional, network-free HTTP fixture
// backed by recorder HAR evidence.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/mgurevin/recorder/hario"
	"github.com/mgurevin/recorder/hartest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: har-fixture capture.har")
		os.Exit(2)
	}

	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open fixture: %w", err)
	}

	document, err := hario.ReadHAR(file, hario.DefaultReadConfig())
	closeErr := file.Close()

	if err != nil {
		return fmt.Errorf("read fixture: %w", err)
	}

	if closeErr != nil {
		return fmt.Errorf("close fixture: %w", closeErr)
	}

	fixture, err := hartest.NewTransport(document.Log.Entries, hartest.DefaultConfig())
	if err != nil {
		return fmt.Errorf("create fixture transport: %w", err)
	}

	client := &http.Client{Transport: fixture}

	response, err := client.Get("https://api.example.test/orders/42")
	if err != nil {
		return fmt.Errorf("execute fixture request: %w", err)
	}

	body, readErr := io.ReadAll(response.Body)
	bodyCloseErr := response.Body.Close()

	if readErr != nil {
		return fmt.Errorf("read fixture response: %w", readErr)
	}

	if bodyCloseErr != nil {
		return fmt.Errorf("close fixture response: %w", bodyCloseErr)
	}

	fmt.Printf("%s\n%s\n", response.Status, body)

	return fixture.Verify()
}
