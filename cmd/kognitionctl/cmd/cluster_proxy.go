// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A cluster service host looks like:
//
//	<svc>.<ns>.svc.cluster.local[:port]
//	<svc>.<ns>.svc[:port]
//
// We route these through the k8s apiserver proxy so studio-run-on-workstation
// can reach services that are only resolvable inside cluster DNS.
const (
	clusterDomainSuffixFull  = ".svc.cluster.local"
	clusterDomainSuffixShort = ".svc"
)

// clusterProxyRoundTripper rewrites requests whose host is an in-cluster
// service DNS name to hit the k8s apiserver's `/services/.../proxy` subresource
// using an authenticated apiserver transport. All other requests pass through
// to `passthrough` unchanged.
type clusterProxyRoundTripper struct {
	apiserver   http.RoundTripper // authenticated transport for the k8s apiserver
	apiHost     string            // e.g. https://kubernetes.docker.internal:6443
	passthrough http.RoundTripper // used when the URL is not an in-cluster service
}

func (t *clusterProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	svc, ns, port, ok := parseClusterServiceHost(req.URL)
	if !ok {
		return t.passthrough.RoundTrip(req)
	}

	// k8s service-proxy path: /api/v1/namespaces/<ns>/services/[<scheme>:]<svc>[:<port>]/proxy/<path>
	// The scheme prefix (http:/https:) is only required for https; for plain http we can omit it.
	svcRef := svc
	if req.URL.Scheme == "https" {
		svcRef = "https:" + svc
	}
	if port != "" {
		svcRef = svcRef + ":" + port
	}

	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	proxied := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s/proxy%s",
		strings.TrimRight(t.apiHost, "/"), ns, svcRef, path)
	if req.URL.RawQuery != "" {
		proxied += "?" + req.URL.RawQuery
	}
	newURL, err := url.Parse(proxied)
	if err != nil {
		return nil, fmt.Errorf("cluster proxy rewrite: %w", err)
	}

	req2 := req.Clone(req.Context())
	req2.URL = newURL
	req2.Host = newURL.Host
	// The apiserver transport injects the bearer token itself; strip any that
	// might have been set upstream so we don't override it.
	req2.Header.Del("Authorization")
	return t.apiserver.RoundTrip(req2)
}

// parseClusterServiceHost returns svc, ns, port when u.Host is a recognized
// in-cluster service DNS name. port is "" when the URL did not include one.
func parseClusterServiceHost(u *url.URL) (svc, ns, port string, ok bool) {
	host := u.Hostname()
	port = u.Port()

	var stem string
	switch {
	case strings.HasSuffix(host, clusterDomainSuffixFull):
		stem = strings.TrimSuffix(host, clusterDomainSuffixFull)
	case strings.HasSuffix(host, clusterDomainSuffixShort):
		stem = strings.TrimSuffix(host, clusterDomainSuffixShort)
	default:
		return "", "", "", false
	}
	parts := strings.Split(stem, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], port, true
}

var (
	clusterProxyOnce   sync.Once
	clusterProxyClient *http.Client
)

// clusterAwareHTTPClient returns an *http.Client that routes requests targeting
// cluster-internal service DNS names (*.svc.cluster.local, *.svc) through the
// k8s apiserver service proxy, using credentials from the current kubeconfig.
//
// When k8s mode is not active or the kubeconfig cannot be loaded, it returns a
// plain client — callers will then simply fail the DNS lookup as before.
//
// The returned client is shared and safe for concurrent use.
func clusterAwareHTTPClient() *http.Client {
	clusterProxyOnce.Do(func() {
		if !k8sMode {
			clusterProxyClient = &http.Client{Timeout: 30 * time.Second}
			return
		}
		apiHost, apiClient, err := resolveKubeAPITransport(kubeContext)
		if err != nil || apiClient == nil || apiClient.Transport == nil {
			clusterProxyClient = &http.Client{Timeout: 30 * time.Second}
			return
		}
		clusterProxyClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &clusterProxyRoundTripper{
				apiserver:   apiClient.Transport,
				apiHost:     apiHost,
				passthrough: http.DefaultTransport,
			},
		}
	})
	return clusterProxyClient
}
