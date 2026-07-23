package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
)

type serverTLSFlags struct {
	certificate *string
	key         *string
}

func addServerTLSFlags(flags *flag.FlagSet) serverTLSFlags {
	return serverTLSFlags{
		certificate: flags.String("tls-cert", "", "PEM server certificate for HTTPS"),
		key:         flags.String("tls-key", "", "PEM private key for HTTPS"),
	}
}

func (f serverTLSFlags) load() (*tls.Config, string, error) {
	certificateFile := *f.certificate

	keyFile := *f.key
	if certificateFile == "" && keyFile == "" {
		return nil, "http", nil
	}

	if certificateFile == "" || keyFile == "" {
		return nil, "", fmt.Errorf("%w: -tls-cert and -tls-key must be provided together", errUsage)
	}

	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		return nil, "", fmt.Errorf("load TLS certificate and key: %w", err)
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}, "https", nil
}

func serveHTTP(server *http.Server, listener net.Listener, config *tls.Config) error {
	if config == nil {
		return server.Serve(listener)
	}

	server.TLSConfig = config

	return server.ServeTLS(listener, "", "")
}
