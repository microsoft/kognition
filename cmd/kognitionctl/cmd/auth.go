// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// apiTokenEnv names the environment variable holding a bearer token for a
// standalone API server that has authentication enabled.
const apiTokenEnv = "KOGNITION_API_TOKEN"

// apiTokenFile optionally names a file containing the bearer token. It keeps
// the secret out of process arguments and shell history.
var apiTokenFile string

// bearerRoundTripper adds the configured API token only to requests for the
// exact server origin. Redirects to other hosts and requests that already carry
// credentials are passed through unchanged.
type bearerRoundTripper struct {
	base   http.RoundTripper
	token  string
	scheme string
	host   string
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" || req.URL.Scheme != rt.scheme || !strings.EqualFold(req.URL.Host, rt.host) {
		return rt.base.RoundTrip(req)
	}
	authorized := req.Clone(req.Context())
	authorized.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(authorized)
}

func resolveAPIToken() (string, error) {
	token := strings.TrimSpace(os.Getenv(apiTokenEnv))
	if apiTokenFile != "" {
		content, err := os.ReadFile(apiTokenFile)
		if err != nil {
			return "", fmt.Errorf("read --token-file: %w", err)
		}
		token = strings.TrimSpace(string(content))
		if token == "" {
			return "", errors.New("--token-file is empty")
		}
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("the API token must be a single line without whitespace")
	}
	return token, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// configureStandaloneAuth wraps httpClient so standalone commands can reach an
// API server started with authentication. It is idempotent and does nothing when
// no token is configured, preserving the unauthenticated local workflow.
func configureStandaloneAuth() error {
	token, err := resolveAPIToken()
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	target, err := url.Parse(serverURL)
	if err != nil || target.Host == "" {
		return errors.New("--server must be an absolute URL when an API token is configured")
	}
	if target.Scheme != "https" && (target.Scheme != "http" || !isLoopbackHost(target.Hostname())) {
		return errors.New("refusing to send an API token over plaintext HTTP to a non-loopback server; use an https --server URL")
	}
	current := httpClient
	if current == nil {
		current = http.DefaultClient
	}
	base := current.Transport
	if wrapped, ok := base.(*bearerRoundTripper); ok {
		base = wrapped.base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	configured := *current
	configured.Transport = &bearerRoundTripper{base: base, token: token, scheme: target.Scheme, host: target.Host}
	httpClient = &configured
	return nil
}
