// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func TestWorkloadIdentityStatusRequiresPinnedController(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	source := testWorkloadSource()
	source.OnX509ContextUpdate(issuer.material(issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))))
	controllerID := "spiffe://kognition.dev/controller/identity-status"
	controller := issuer.issue(test, controllerID, time.Now().Add(time.Hour))
	handler, err := NewWorkloadIdentityStatusHandler(source, controllerID)
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/workload-identity/status", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: controller.Certificates}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var status WorkloadIdentityStatus
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil || !status.Ready || status.SPIFFEID != testIncarnationID {
		test.Fatalf("verified metadata status=%d body=%s", response.Code, response.Body)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 7 {
		test.Fatal("status response must contain only the seven public metadata fields")
	}
	for _, scenario := range []struct{ name, peer string }{
		{"another controller", "spiffe://kognition.dev/controller/another"},
		{"tenant workload", testIncarnationID},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/workload-identity/status", nil)
			request.TLS = &tls.ConnectionState{PeerCertificates: issuer.issue(test, scenario.peer, time.Now().Add(time.Hour)).Certificates}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				test.Fatalf("unpinned peer accepted: %d", response.Code)
			}
		})
	}
	request.TLS = nil
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		test.Fatal("plaintext status request accepted")
	}
	source.OnX509ContextWatchError(ErrWorkloadIdentityUnavailable)
	request.TLS = &tls.ConnectionState{PeerCertificates: controller.Certificates}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		test.Fatal("issuer outage returned cached ready metadata")
	}
	if _, err := NewWorkloadIdentityStatusHandler(source, spiffeid.RequireFromString("spiffe://untrusted.dev/controller/status").String()); err == nil {
		test.Fatal("controller from another trust domain accepted")
	}
}

func TestWorkloadIdentityStatusClientUsesVerifiedMTLS(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	workloadSource := testWorkloadSource()
	workloadCertificate := issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))
	workloadSource.OnX509ContextUpdate(issuer.material(workloadCertificate))
	controllerID := "spiffe://kognition.dev/controller/identity-status"
	controllerSource := &WorkloadIdentity{expected: spiffeid.RequireFromString(controllerID), initial: make(chan struct{}, 1), status: WorkloadIdentityStatus{SPIFFEID: controllerID, TrustDomain: "kognition.dev"}}
	controllerSource.OnX509ContextUpdate(issuer.material(issuer.issue(test, controllerID, time.Now().Add(time.Hour))))
	configured, err := NewWorkloadIdentityStatusServer(workloadSource, controllerID)
	if err != nil {
		test.Fatal(err)
	}
	server := httptest.NewUnstartedServer(configured.Handler)
	server.Listener = tls.NewListener(server.Listener, configured.TLSConfig)
	server.Start()
	defer server.Close()
	endpoint := strings.Replace(server.URL, "http://", "https://", 1)
	client, err := NewWorkloadIdentityStatusClient(controllerSource, endpoint, "kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000001")
	if err != nil {
		test.Fatal(err)
	}
	defer client.Close()
	status, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001")
	if err != nil || !status.Ready || status.Generation != 1 {
		test.Fatalf("verified network status=%+v error=%v", status, err)
	}
	workloadSource.OnX509ContextUpdate(issuer.material(issuer.issue(test, testIncarnationID, time.Now().Add(2*time.Hour))))
	if status, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001"); err != nil || status.Generation != 2 {
		test.Fatalf("rotation was not refreshed: status=%+v error=%v", status, err)
	}
	if status, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000002"); !errors.Is(err, ErrWorkloadIdentityUnavailable) || status != (WorkloadIdentityStatus{}) {
		test.Fatal("recreated Agent received cached certificate metadata")
	}
	wrongClient, err := NewWorkloadIdentityStatusClient(controllerSource, endpoint, "kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000002")
	if err != nil {
		test.Fatal(err)
	}
	defer wrongClient.Close()
	if _, err := wrongClient.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000002"); !errors.Is(err, ErrWorkloadIdentityUnavailable) {
		test.Fatal("TLS accepted the old Agent server for a recreated UID")
	}
	for _, invalidEndpoint := range []string{"http://localhost", "https://user:password@localhost", endpoint + "/extra", endpoint + "?query=1", endpoint + "#fragment"} {
		if _, err := NewWorkloadIdentityStatusClient(controllerSource, invalidEndpoint, "kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000001"); err == nil {
			test.Fatal("non-fixed status endpoint accepted")
		}
	}
	valid, err := json.Marshal(workloadSource.Status())
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct{ name, body string }{
		{"duplicate", strings.TrimSuffix(string(valid), "}") + `,"Ready":true}`},
		{"unknown field", strings.TrimSuffix(string(valid), "}") + `,"PrivateKey":"not-allowed"}`},
		{"missing", `{}`},
		{"trailing", string(valid) + `{}`},
		{"null", strings.Replace(string(valid), `"Ready":true`, `"Ready":null`, 1)},
		{"oversized", strings.Repeat(" ", 8193) + string(valid)},
		{"wrong incarnation", strings.Replace(string(valid), "000000000001", "000000000002", 1)},
		{"false readiness", strings.Replace(string(valid), `"Ready":true`, `"Ready":false`, 1)},
		{"forged validity", strings.Replace(string(valid), workloadSource.Status().ExpiresAt.Format(time.RFC3339), time.Now().Add(3*time.Hour).UTC().Format(time.RFC3339), 1)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(scenario.body)) })
			if status, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrWorkloadIdentityUnavailable) || status != (WorkloadIdentityStatus{}) {
				test.Fatal("malformed status produced certificate readiness")
			}
		})
	}
	redirectContacts := 0
	redirect := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectContacts++ }))
	defer redirect.Close()
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, redirect.URL, http.StatusTemporaryRedirect)
	})
	if _, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrWorkloadIdentityUnavailable) || redirectContacts != 0 {
		test.Fatal("status redirect reached another endpoint")
	}
	server.Config.Handler = configured.Handler
	workloadSource.OnX509ContextWatchError(ErrWorkloadIdentityUnavailable)
	if status, err := client.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001"); !errors.Is(err, ErrWorkloadIdentityUnavailable) || status != (WorkloadIdentityStatus{}) {
		test.Fatal("issuer outage returned cached network status")
	}
}
