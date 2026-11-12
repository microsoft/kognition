// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const testAPIToken = "test-api-token-value"

func useStandaloneAuthState(t *testing.T, server string) {
	t.Helper()
	previousMode, previousK8s, previousURL, previousClient, previousFile := cliMode, k8sMode, serverURL, httpClient, apiTokenFile
	cliMode, k8sMode, serverURL, httpClient, apiTokenFile = "standalone", false, server, http.DefaultClient, ""
	t.Setenv(apiTokenEnv, "")
	t.Cleanup(func() {
		cliMode, k8sMode, serverURL, httpClient, apiTokenFile = previousMode, previousK8s, previousURL, previousClient, previousFile
	})
}

func authorizationSeenBy(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		if seen != "Bearer "+testAPIToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func TestStandaloneWithoutTokenSendsNoCredentialsAndKeepsClient(t *testing.T) {
	server, seen := authorizationSeenBy(t)
	useStandaloneAuthState(t, server.URL)
	before := httpClient

	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatal(err)
	}
	if httpClient != before {
		t.Fatal("the unauthenticated local workflow must keep its original client")
	}
	response, err := httpClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if *seen != "" || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("authorization=%q status=%d", *seen, response.StatusCode)
	}
}

func TestStandaloneEnvironmentTokenAuthenticatesAgainstProtectedServer(t *testing.T) {
	server, seen := authorizationSeenBy(t)
	useStandaloneAuthState(t, server.URL)
	t.Setenv(apiTokenEnv, testAPIToken+"\n")

	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatal(err)
	}
	response, err := httpClient.Get(server.URL + "/apis/kognition.dev/v1alpha1/namespaces/default/agents")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || *seen != "Bearer "+testAPIToken {
		t.Fatalf("status=%d authorization=%q", response.StatusCode, *seen)
	}
}

func TestStandaloneTokenFileTakesPrecedenceAndRejectsEmptyFiles(t *testing.T) {
	server, seen := authorizationSeenBy(t)
	useStandaloneAuthState(t, server.URL)
	t.Setenv(apiTokenEnv, "environment-token")
	apiTokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(apiTokenFile, []byte(testAPIToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatal(err)
	}
	response, err := httpClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if *seen != "Bearer "+testAPIToken {
		t.Fatalf("token file was not used: %q", *seen)
	}

	if err := os.WriteFile(apiTokenFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolveMode(&cobra.Command{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty token file error = %v", err)
	}
	apiTokenFile = filepath.Join(t.TempDir(), "missing")
	if err := resolveMode(&cobra.Command{}); err == nil {
		t.Fatal("missing token file accepted")
	}
}

func TestStandaloneTokenIsNeverSentToOtherHostsOrOverridden(t *testing.T) {
	server, seen := authorizationSeenBy(t)
	var other string
	otherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other = r.Header.Get("Authorization")
	}))
	t.Cleanup(otherServer.Close)
	useStandaloneAuthState(t, server.URL)
	t.Setenv(apiTokenEnv, testAPIToken)
	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatal(err)
	}

	response, err := httpClient.Get(otherServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if other != "" {
		t.Fatalf("token leaked to another origin: %q", other)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer caller-supplied")
	response, err = httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if *seen != "Bearer caller-supplied" {
		t.Fatalf("explicit caller credentials were overridden: %q", *seen)
	}
}

func TestStandaloneTokenRefusesPlaintextRemoteAndMalformedConfiguration(t *testing.T) {
	useStandaloneAuthState(t, "http://api.example.test")
	t.Setenv(apiTokenEnv, testAPIToken)
	err := resolveMode(&cobra.Command{})
	if err == nil || !strings.Contains(err.Error(), "plaintext") || strings.Contains(err.Error(), testAPIToken) {
		t.Fatalf("plaintext remote error = %v", err)
	}

	serverURL = "https://api.example.test"
	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatalf("https remote must be accepted: %v", err)
	}
	for _, loopback := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		serverURL = loopback
		if err := resolveMode(&cobra.Command{}); err != nil {
			t.Fatalf("loopback %s rejected: %v", loopback, err)
		}
	}
	serverURL = "://invalid"
	if err := resolveMode(&cobra.Command{}); err == nil {
		t.Fatal("invalid server URL accepted with a token")
	}
	serverURL = "https://api.example.test"
	t.Setenv(apiTokenEnv, "two words")
	if err := resolveMode(&cobra.Command{}); err == nil || strings.Contains(err.Error(), "two words") {
		t.Fatalf("whitespace token error = %v", err)
	}
}

func TestStandaloneAuthReconfigurationDoesNotStackWrappers(t *testing.T) {
	server, seen := authorizationSeenBy(t)
	useStandaloneAuthState(t, server.URL)
	t.Setenv(apiTokenEnv, testAPIToken)
	for range 3 {
		if err := resolveMode(&cobra.Command{}); err != nil {
			t.Fatal(err)
		}
	}
	wrapper, ok := httpClient.Transport.(*bearerRoundTripper)
	if !ok {
		t.Fatalf("transport = %T", httpClient.Transport)
	}
	if _, nested := wrapper.base.(*bearerRoundTripper); nested {
		t.Fatal("authentication wrappers were stacked")
	}
	response, err := httpClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if *seen != "Bearer "+testAPIToken {
		t.Fatalf("authorization = %q", *seen)
	}
}
