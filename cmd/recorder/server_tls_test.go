package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerTLSFlags(t *testing.T) {
	certificateFile, keyFile, certificate := writeServerKeyPair(t)

	flags := flag.NewFlagSet("tls", flag.ContinueOnError)

	tlsFlags := addServerTLSFlags(flags)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}

	config, scheme, err := tlsFlags.load()
	if err != nil {
		t.Fatal(err)
	}

	if config != nil || scheme != "http" {
		t.Fatalf("plain config = %#v, %q", config, scheme)
	}

	flags = flag.NewFlagSet("tls", flag.ContinueOnError)

	tlsFlags = addServerTLSFlags(flags)
	if err := flags.Parse([]string{"-tls-cert", certificateFile}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := tlsFlags.load(); !errors.Is(err, errUsage) {
		t.Fatalf("unpaired TLS option error = %v", err)
	}

	flags = flag.NewFlagSet("tls", flag.ContinueOnError)

	tlsFlags = addServerTLSFlags(flags)
	if err := flags.Parse([]string{
		"-tls-cert", certificateFile,
		"-tls-key", keyFile,
	}); err != nil {
		t.Fatal(err)
	}

	config, scheme, err = tlsFlags.load()
	if err != nil {
		t.Fatal(err)
	}

	if scheme != "https" || config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config = %#v, %q", config, scheme)
	}

	if len(config.Certificates) != 1 {
		t.Fatalf("certificates = %d", len(config.Certificates))
	}

	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	testTLSServer(t, config, pool)
}

func TestServerTLSFlagsRejectMalformedKeyPair(t *testing.T) {
	certificateFile, _, _ := writeServerKeyPair(t)

	flags := flag.NewFlagSet("tls", flag.ContinueOnError)

	tlsFlags := addServerTLSFlags(flags)
	if err := flags.Parse([]string{
		"-tls-cert", certificateFile,
		"-tls-key", certificateFile,
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := tlsFlags.load(); err == nil {
		t.Fatal("malformed TLS key pair succeeded")
	}
}

func TestTLSCommandURLs(t *testing.T) {
	certificateFile, keyFile, _ := writeServerKeyPair(t)
	capturePath := writeCapture(t, "capture.har", testHAR(t))

	inspectContext, cancelInspect := context.WithCancel(context.Background())
	cancelInspect()

	var inspectOutput strings.Builder
	if err := run(
		inspectContext,
		[]string{
			"inspect",
			"-no-open",
			"-inspector-url", "http://localhost:5173/",
			"-tls-cert", certificateFile,
			"-tls-key", keyFile,
			capturePath,
		},
		&inspectOutput,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(inspectOutput.String(), "https%3A%2F%2F127.0.0.1") {
		t.Fatalf("inspect output = %q", inspectOutput.String())
	}

	fixtureContext, cancelFixture := context.WithCancel(context.Background())
	cancelFixture()

	var fixtureOutput strings.Builder
	if err := run(
		fixtureContext,
		[]string{
			"serve-fixture",
			"-listen", "127.0.0.1:0",
			"-allow-unused",
			"-tls-cert", certificateFile,
			"-tls-key", keyFile,
			capturePath,
		},
		&fixtureOutput,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(fixtureOutput.String(), " at https://127.0.0.1:") {
		t.Fatalf("fixture output = %q", fixtureOutput.String())
	}
}

func testTLSServer(t *testing.T, config *tls.Config, roots *x509.CertPool) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, "ok")
		}),
		ReadHeaderTimeout: time.Second,
	}

	defer func() {
		_ = server.Close()
	}()

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- serveHTTP(server, listener, config)
	}()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
			},
		},
	}

	response, err := client.Get("https://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve error = %v", err)
	}
}

func writeServerKeyPair(t *testing.T) (string, string, *x509.Certificate) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		publicKey,
		privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}

	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatal(err)
	}

	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	certificateFile := filepath.Join(root, "server.crt")
	keyFile := filepath.Join(root, "server.key")

	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificateDER,
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privateKeyDER,
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	return certificateFile, keyFile, certificate
}
