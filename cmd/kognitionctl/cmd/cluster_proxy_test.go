// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestParseClusterServiceHost(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		wantSvc  string
		wantNS   string
		wantPort string
		wantOK   bool
	}{
		{name: "full cluster domain", rawURL: "http://api.kognition.svc.cluster.local", wantSvc: "api", wantNS: "kognition", wantOK: true},
		{name: "short domain with port", rawURL: "https://runtime.default.svc:8443", wantSvc: "runtime", wantNS: "default", wantPort: "8443", wantOK: true},
		{name: "public host", rawURL: "https://api.example.com"},
		{name: "missing namespace", rawURL: "http://api.svc"},
		{name: "extra domain label", rawURL: "http://api.default.extra.svc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parse test URL: %v", err)
			}
			svc, ns, port, ok := parseClusterServiceHost(u)
			if svc != tt.wantSvc || ns != tt.wantNS || port != tt.wantPort || ok != tt.wantOK {
				t.Fatalf("parseClusterServiceHost() = (%q, %q, %q, %v), want (%q, %q, %q, %v)", svc, ns, port, ok, tt.wantSvc, tt.wantNS, tt.wantPort, tt.wantOK)
			}
		})
	}
}

func TestClusterProxyRoundTripper_RewritesClusterServiceRequest(t *testing.T) {
	var proxied *http.Request
	transport := &clusterProxyRoundTripper{
		apiHost: "https://cluster.example.test/",
		apiserver: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxied = req
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
		passthrough: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("cluster request used passthrough transport")
			return nil, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "https://runtime.default.svc:8443/v1/invoke?stream=true", strings.NewReader("request"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer caller-token")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if proxied == nil {
		t.Fatal("apiserver transport did not receive request")
	}
	wantURL := "https://cluster.example.test/api/v1/namespaces/default/services/https:runtime:8443/proxy/v1/invoke?stream=true"
	if proxied.URL.String() != wantURL {
		t.Fatalf("proxied URL = %q, want %q", proxied.URL, wantURL)
	}
	if proxied.Host != "cluster.example.test" {
		t.Fatalf("proxied Host = %q, want %q", proxied.Host, "cluster.example.test")
	}
	if got := proxied.Header.Get("Authorization"); got != "" {
		t.Fatalf("proxied Authorization = %q, want empty", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer caller-token" {
		t.Fatalf("original Authorization = %q, want request unchanged", got)
	}
}

func TestClusterProxyRoundTripper_RewritesHTTPRootRequest(t *testing.T) {
	var proxied *http.Request
	transport := &clusterProxyRoundTripper{
		apiHost: "https://cluster.example.test",
		apiserver: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxied = req
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
		passthrough: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("cluster request used passthrough transport")
			return nil, nil
		}),
	}
	req, err := http.NewRequest(http.MethodGet, "http://agent.platform.svc?ready=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	wantURL := "https://cluster.example.test/api/v1/namespaces/platform/services/agent/proxy/?ready=true"
	if proxied == nil || proxied.URL.String() != wantURL {
		t.Fatalf("proxied URL = %v, want %q", proxied, wantURL)
	}
}

func TestClusterProxyRoundTripper_PassesThroughExternalRequest(t *testing.T) {
	var passed *http.Request
	transport := &clusterProxyRoundTripper{
		apiHost: "https://cluster.example.test",
		apiserver: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("external request used apiserver transport")
			return nil, nil
		}),
		passthrough: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			passed = req
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer external-token")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if passed != req {
		t.Fatal("passthrough transport did not receive original request")
	}
	if got := passed.Header.Get("Authorization"); got != "Bearer external-token" {
		t.Fatalf("passthrough Authorization = %q, want caller token preserved", got)
	}
}
