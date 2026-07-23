package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hartest"
)

func runServeFixture(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("serve-fixture", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")
	listenAddress := flags.String("listen", "127.0.0.1:8080", "loopback listen address")
	originValue := flags.String("origin", "", "recorded origin to map incoming paths onto")
	bodyStore := flags.String("body-store", "", "FileBodyStore root containing assets/")
	allowUnused := flags.Bool("allow-unused", false, "exit successfully when fixtures remain unused")
	serverTLS := addServerTLSFlags(flags)

	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	path, err := onePath(flags)
	if err != nil {
		return err
	}

	_, entries, _, err := readCapture(path, *inputFormat)
	if err != nil {
		return err
	}

	origin, err := fixtureOrigin(entries, *originValue)
	if err != nil {
		return err
	}

	config := hartest.DefaultConfig()
	if *bodyStore != "" {
		config.Bodies = diskBodyOpener{root: *bodyStore}
	}

	fixture, err := hartest.NewTransport(entries, config)
	if err != nil {
		return fmt.Errorf("create fixture transport: %w", err)
	}

	tlsConfig, serverScheme, err := serverTLS.load()
	if err != nil {
		return err
	}

	listener, err := listenLoopback(*listenAddress)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
	}()

	server := &http.Server{
		Handler:           fixtureHandler(fixture, origin, stderr),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErrors := make(chan error, 1)

	go func() {
		serveErr := serveHTTP(server, listener, tlsConfig)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}

		serveErrors <- serveErr
	}()

	if _, err := fmt.Fprintf(
		stdout,
		"Serving %d fixtures at %s://%s mapped to %s; press Ctrl-C to stop.\n",
		len(entries),
		serverScheme,
		listener.Addr().String(),
		origin.String(),
	); err != nil {
		return err
	}

	signalContext, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	select {
	case serveErr := <-serveErrors:
		if serveErr != nil {
			return fmt.Errorf("serve fixture: %w", serveErr)
		}

	case <-signalContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("stop fixture server: %w", err)
		}

		if serveErr := <-serveErrors; serveErr != nil {
			return fmt.Errorf("serve fixture: %w", serveErr)
		}
	}

	if !*allowUnused {
		if err := fixture.Verify(); err != nil {
			return err
		}
	}

	return nil
}

func fixtureOrigin(entries []*recorder.Entry, requested string) (*url.URL, error) {
	if requested != "" {
		parsed, err := url.Parse(requested)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid fixture origin: %v", errUsage, err)
		}

		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("%w: fixture origin must use http or https", errUsage)
		}

		if parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("%w: fixture origin must contain only scheme and host", errUsage)
		}

		return parsed, nil
	}

	origins := make(map[string]*url.URL)

	for _, entry := range entries {
		if entry == nil || entry.Request == nil {
			continue
		}

		parsed, err := url.Parse(entry.Request.URL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}

		key := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
		origins[key] = &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	}

	if len(origins) != 1 {
		keys := make([]string, 0, len(origins))
		for key := range origins {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		return nil, fmt.Errorf(
			"%w: fixture contains %d origins (%s); select entries first or use -origin",
			errUsage,
			len(origins),
			strings.Join(keys, ", "),
		)
	}

	for _, origin := range origins {
		return origin, nil
	}

	return nil, fmt.Errorf("%w: fixture contains no request origin", errUsage)
}

func listenLoopback(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: listen address must be host:port", errUsage)
	}

	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("%w: serve-fixture only listens on loopback", errUsage)
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}

	return listener, nil
}

func fixtureHandler(transport http.RoundTripper, origin *url.URL, stderr io.Writer) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetRequest := request.Clone(request.Context())
		targetURL := *request.URL
		targetURL.Scheme = origin.Scheme
		targetURL.Host = origin.Host
		targetRequest.URL = &targetURL
		targetRequest.Host = origin.Host
		targetRequest.RequestURI = ""

		response, err := transport.RoundTrip(targetRequest)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "recorder: fixture mismatch: %v\n", err)

			http.Error(writer, "fixture request did not match", http.StatusBadGateway)

			return
		}
		defer func() {
			_ = response.Body.Close()
		}()

		copyFixtureHeaders(writer.Header(), response.Header)

		for name := range response.Trailer {
			writer.Header().Add("Trailer", name)
		}

		writer.WriteHeader(response.StatusCode)

		if _, err := io.Copy(writer, response.Body); err != nil {
			_, _ = fmt.Fprintf(stderr, "recorder: fixture response body: %v\n", err)

			return
		}

		for name, values := range response.Trailer {
			writer.Header()[http.TrailerPrefix+name] = append([]string(nil), values...)
		}
	})
}

func copyFixtureHeaders(destination, source http.Header) {
	for name, values := range source {
		if isHopByHopHeader(name) {
			continue
		}

		destination[name] = append([]string(nil), values...)
	}
}

func isHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true

	default:
		return false
	}
}

type diskBodyOpener struct {
	root string
}

func (o diskBodyOpener) Open(reference string) (io.ReadCloser, error) {
	data, err := readBodyAsset(o.root, reference)
	if err != nil {
		return nil, err
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}
