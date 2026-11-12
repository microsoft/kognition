// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

const testIncarnationID = "spiffe://kognition.dev/workspace/tenant/agent/worker/incarnation/70000000-0000-4000-8000-000000000001"

func TestVerifiedWorkloadPeerRequiresCurrentAgentIncarnation(test *testing.T) {
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "worker", Namespace: "tenant", UID: "70000000-0000-4000-8000-000000000001"}}
	if err := RequireAgentWorkloadPeer(context.Background(), agent); err != nil {
		test.Fatal("legacy capability profile was changed")
	}
	ctx := context.WithValue(context.Background(), workloadPeerKey{}, spiffeid.RequireFromString(testIncarnationID))
	if err := RequireAgentWorkloadPeer(ctx, agent); err != nil {
		test.Fatal(err)
	}
	agent.ObjectMeta.UID = "70000000-0000-4000-8000-000000000002"
	if err := RequireAgentWorkloadPeer(ctx, agent); err == nil {
		test.Fatal("recreated Agent accepted old workload identity")
	}
	agent.ObjectMeta.UID = "70000000-0000-4000-8000-000000000001"
	agent.ObjectMeta.Namespace = "another-tenant"
	if err := RequireAgentWorkloadPeer(ctx, agent); err == nil {
		test.Fatal("another namespace accepted workload identity")
	}
}

type spiffeTestIssuer struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	serial      int64
}

func newSPIFFETestIssuer(test *testing.T) *spiffeTestIssuer {
	test.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated test issuer"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		test.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(encoded)
	if err != nil {
		test.Fatal(err)
	}
	return &spiffeTestIssuer{certificate: parsed, key: key, serial: 1}
}

func (issuer *spiffeTestIssuer) issue(test *testing.T, identity string, expiry time.Time) *x509svid.SVID {
	test.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		test.Fatal(err)
	}
	issuer.serial++
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(issuer.serial), URIs: []*url.URL{uri},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: expiry,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, issuer.certificate, &key.PublicKey, issuer.key)
	if err != nil {
		test.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(encoded)
	if err != nil {
		test.Fatal(err)
	}
	return &x509svid.SVID{ID: spiffeid.RequireFromString(identity), Certificates: []*x509.Certificate{parsed}, PrivateKey: key}
}

func (issuer *spiffeTestIssuer) material(svid *x509svid.SVID) *workloadapi.X509Context {
	bundle := x509bundle.FromX509Authorities(svid.ID.TrustDomain(), []*x509.Certificate{issuer.certificate})
	return &workloadapi.X509Context{SVIDs: []*x509svid.SVID{svid}, Bundles: x509bundle.NewSet(bundle)}
}

func testWorkloadSource() *WorkloadIdentity {
	return &WorkloadIdentity{
		expected: spiffeid.RequireFromString(testIncarnationID), initial: make(chan struct{}, 1),
		status: WorkloadIdentityStatus{SPIFFEID: testIncarnationID, TrustDomain: "kognition.dev"},
	}
}

func TestWorkloadIdentityAgentStatusBindsCurrentIncarnation(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	source := testWorkloadSource()
	source.OnX509ContextUpdate(issuer.material(issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))))
	status, err := source.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001")
	if err != nil || !status.Ready || status.SPIFFEID != testIncarnationID || status.ExpiresAt.IsZero() || status.Generation != 1 {
		test.Fatalf("verified Agent status = %+v, error = %v", status, err)
	}
	for _, target := range []struct{ namespace, name, uid string }{
		{"tenant", "worker", "70000000-0000-4000-8000-000000000002"},
		{"another", "worker", "70000000-0000-4000-8000-000000000001"},
		{"tenant", "another", "70000000-0000-4000-8000-000000000001"},
		{"tenant", "worker", ""},
	} {
		status, err := source.AgentStatus(target.namespace, target.name, target.uid)
		if !errors.Is(err, ErrWorkloadIdentityUnavailable) || status != (WorkloadIdentityStatus{}) {
			test.Fatalf("mismatched Agent received evidence: status=%+v error=%v", status, err)
		}
	}
	status.Ready = false
	if current, err := source.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001"); err != nil || !current.Ready {
		test.Fatal("caller changed the source's verified readiness")
	}
	source.OnX509ContextWatchError(errors.New("issuer outage"))
	status, err = source.AgentStatus("tenant", "worker", "70000000-0000-4000-8000-000000000001")
	if err != nil || status.Ready || status.Reason != "IssuerUnavailable" {
		test.Fatalf("issuer outage status = %+v, error = %v", status, err)
	}
}

func TestAgentIncarnationSPIFFEID(test *testing.T) {
	identity, err := AgentIncarnationSPIFFEID("kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000001")
	if err != nil || identity != testIncarnationID {
		test.Fatalf("identity=%q error=%v", identity, err)
	}
	for _, invalid := range []struct{ domain, namespace, name, uid string }{
		{"https://kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000001"},
		{"kognition.dev", "../tenant", "worker", "70000000-0000-4000-8000-000000000001"},
		{"kognition.dev", "tenant", "worker/other", "70000000-0000-4000-8000-000000000001"},
		{"kognition.dev", "tenant", "worker", ""},
		{"kognition.dev", "tenant", "worker", "00000000-0000-0000-0000-000000000000"},
		{"kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000001/other"},
	} {
		if _, err := AgentIncarnationSPIFFEID(invalid.domain, invalid.namespace, invalid.name, invalid.uid); err == nil {
			test.Fatalf("accepted malformed incarnation: %+v", invalid)
		}
	}
	second, err := AgentIncarnationSPIFFEID("kognition.dev", "tenant", "worker", "70000000-0000-4000-8000-000000000002")
	if err != nil || second == identity {
		test.Fatal("Agent recreation reused workload identity")
	}
}

func TestWorkloadIdentityValidationAndRotation(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	source := testWorkloadSource()
	first := issuer.issue(test, testIncarnationID, time.Now().Add(20*time.Minute))
	source.OnX509ContextUpdate(issuer.material(first))
	if !source.Status().Ready || source.Status().Generation != 1 || source.Status().SPIFFEID != testIncarnationID {
		test.Fatalf("initial status: %+v", source.Status())
	}
	second := issuer.issue(test, testIncarnationID, time.Now().Add(30*time.Minute))
	source.OnX509ContextUpdate(issuer.material(second))
	current, err := source.GetX509SVID()
	if err != nil || current.Certificates[0].SerialNumber.Cmp(second.Certificates[0].SerialNumber) != 0 || source.Status().Generation != 2 {
		test.Fatal("rotation did not replace identity material")
	}
	source.OnX509ContextWatchError(errors.New("sensitive issuer diagnostics"))
	if source.Status().Ready || source.Status().Reason != "IssuerUnavailable" {
		test.Fatalf("issuer outage remained ready: %+v", source.Status())
	}
	if _, err := source.GetX509SVID(); !errors.Is(err, ErrWorkloadIdentityUnavailable) {
		test.Fatal("cached identity used after issuer outage")
	}
	source.OnX509ContextUpdate(issuer.material(second))
	if !source.Status().Ready || source.Status().Generation != 3 {
		test.Fatal("identity did not recover on a verified issuer update")
	}
	for _, testCase := range []struct {
		name, identity string
		expiry         time.Time
	}{
		{"wrong-incarnation", strings.Replace(testIncarnationID, "000000000001", "000000000002", 1), time.Now().Add(time.Hour)},
		{"wrong-domain", strings.Replace(testIncarnationID, "kognition.dev", "untrusted.dev", 1), time.Now().Add(time.Hour)},
		{"expired", testIncarnationID, time.Now().Add(-time.Second)},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			source.OnX509ContextUpdate(issuer.material(issuer.issue(test, testCase.identity, testCase.expiry)))
			if source.Status().Ready {
				test.Fatal("invalid identity marked ready")
			}
		})
	}
	source.OnX509ContextUpdate(nil)
	if source.Status().Ready {
		test.Fatal("empty identity update remained ready")
	}
	mismatchedKey := issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))
	mismatchedKey.PrivateKey = first.PrivateKey
	source.OnX509ContextUpdate(issuer.material(mismatchedKey))
	if source.Status().Ready {
		test.Fatal("certificate and private key mismatch marked ready")
	}
	duplicate := issuer.material(first)
	duplicate.SVIDs = append(duplicate.SVIDs, second)
	source.OnX509ContextUpdate(duplicate)
	if source.Status().Ready {
		test.Fatal("duplicate expected identity was accepted")
	}
}

type spiffeTestSVIDSource struct{ svid *x509svid.SVID }

func (source spiffeTestSVIDSource) GetX509SVID() (*x509svid.SVID, error) { return source.svid, nil }

func TestWorkloadHTTPClientVerifiesPeerAndBlocksOutage(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	source := testWorkloadSource()
	source.OnX509ContextUpdate(issuer.material(issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))))
	peerID := "spiffe://kognition.dev/gateway/model"
	peer := issuer.issue(test, peerID, time.Now().Add(time.Hour))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("verified")) }))
	server.TLS = tlsconfig.MTLSServerConfig(spiffeTestSVIDSource{peer}, issuer.material(peer).Bundles, tlsconfig.AuthorizeID(source.expected))
	server.TLS.Certificates = []tls.Certificate{{Certificate: [][]byte{peer.Certificates[0].Raw}, PrivateKey: peer.PrivateKey}}
	server.TLS.MinVersion = tls.VersionTLS13
	server.StartTLS()
	defer server.Close()
	client, err := source.HTTPClient(peerID)
	if err != nil {
		test.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		test.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	wrongPeer, err := source.HTTPClient("spiffe://kognition.dev/gateway/other")
	if err != nil {
		test.Fatal(err)
	}
	defer wrongPeer.CloseIdleConnections()
	if response, err := wrongPeer.Get(server.URL); err == nil {
		_ = response.Body.Close()
		test.Fatal("TLS accepted a different peer SPIFFE identity")
	}
	if _, err := source.HTTPClient("spiffe://untrusted.dev/gateway/model"); err == nil {
		test.Fatal("accepted a peer from another trust domain")
	}
	source.OnX509ContextWatchError(errors.New("issuer stopped"))
	if response, err := client.Get(server.URL); err == nil {
		_ = response.Body.Close()
		test.Fatal("pooled TLS connection bypassed issuer readiness")
	}
	if response, err := client.Get(strings.Replace(server.URL, "https://", "http://", 1)); err == nil {
		_ = response.Body.Close()
		test.Fatal("workload transport accepted plaintext")
	}
}

func TestWorkloadProxyPinsPeerAndForwardsCapabilities(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	source := testWorkloadSource()
	source.OnX509ContextUpdate(issuer.material(issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))))
	peerID := "spiffe://kognition.dev/gateway/model"
	peer := issuer.issue(test, peerID, time.Now().Add(time.Hour))
	calls := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer scoped-model-capability" {
			test.Error("proxy changed gateway authority or operation")
		}
		if request.Header.Get(observability.TrustDecisionRunIDHeader) != "review-run" || request.Header.Get("traceparent") != "00-11111111111111111111111111111111-2222222222222222-01" {
			test.Error("proxy dropped trusted run or trace correlation")
		}
		_, _ = writer.Write([]byte(`{"verified":true}`))
	}))
	server.TLS = tlsconfig.MTLSServerConfig(spiffeTestSVIDSource{peer}, issuer.material(peer).Bundles, tlsconfig.AuthorizeID(source.expected))
	server.TLS.Certificates = []tls.Certificate{{Certificate: [][]byte{peer.Certificates[0].Raw}, PrivateKey: peer.PrivateKey}}
	server.StartTLS()
	defer server.Close()
	endpoints := map[string]WorkloadGatewayEndpoint{}
	for plane := range workloadOperations {
		endpoints[plane] = WorkloadGatewayEndpoint{URL: server.URL, PeerID: peerID}
	}
	proxy, err := NewWorkloadProxy(source, endpoints)
	if err != nil {
		test.Fatal(err)
	}
	defer proxy.Close()
	invoke := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"bound"}`))
		request.Header.Set("Authorization", "Bearer scoped-model-capability")
		request.Header.Set(observability.TrustDecisionRunIDHeader, "review-run")
		request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		return response
	}
	if response := invoke("/model/v1/chat/completions"); response.Code != http.StatusOK || calls != 1 {
		test.Fatalf("verified gateway proxy status=%d calls=%d", response.Code, calls)
	}
	if response := invoke("/model/admin"); response.Code != http.StatusNotFound || calls != 1 {
		test.Fatal("proxy accepted an unregistered operation")
	}
	wrong := endpoints["model"]
	wrong.PeerID = "spiffe://kognition.dev/gateway/other"
	endpoints["model"] = wrong
	wrongProxy, err := NewWorkloadProxy(source, endpoints)
	if err != nil {
		test.Fatal(err)
	}
	defer wrongProxy.Close()
	request := httptest.NewRequest(http.MethodPost, "/model/v1/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer scoped-model-capability")
	response := httptest.NewRecorder()
	wrongProxy.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || calls != 1 {
		test.Fatal("proxy accepted another gateway peer")
	}
	source.OnX509ContextWatchError(errors.New("issuer offline"))
	if response := invoke("/model/v1/chat/completions"); response.Code != http.StatusUnauthorized || calls != 1 {
		test.Fatal("proxy dispatched while workload identity was unavailable")
	}
}

type testWorkloadAPIServer struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	updates chan *workload.X509SVIDResponse
}

func (server *testWorkloadAPIServer) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	for {
		select {
		case update := <-server.updates:
			if err := stream.Send(update); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func TestWorkloadIdentityUsesOfficialStreamAndCloses(test *testing.T) {
	issuer := newSPIFFETestIssuer(test)
	svid := issuer.issue(test, testIncarnationID, time.Now().Add(time.Hour))
	privateKey, err := x509.MarshalPKCS8PrivateKey(svid.PrivateKey)
	if err != nil {
		test.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	apiServer := &testWorkloadAPIServer{updates: make(chan *workload.X509SVIDResponse, 1)}
	workload.RegisterSpiffeWorkloadAPIServer(server, apiServer)
	go func() { _ = server.Serve(listener) }()
	test.Cleanup(server.Stop)
	apiServer.updates <- &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{SpiffeId: testIncarnationID, X509Svid: svid.Certificates[0].Raw, X509SvidKey: privateKey, Bundle: issuer.certificate.Raw}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := workloadapi.New(ctx, workloadapi.WithAddr("tcp://127.0.0.1:12345"), workloadapi.WithDialOptions(grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() })))
	if err != nil {
		test.Fatal(err)
	}
	source, err := startWorkloadIdentity(ctx, spiffeid.RequireFromString(testIncarnationID), client)
	if err != nil {
		test.Fatal(err)
	}
	if !source.Status().Ready {
		test.Fatal("official Workload API did not produce ready identity")
	}
	if err := source.Close(); err != nil {
		test.Fatal(err)
	}
	if err := source.Close(); err != nil || source.Status().Ready || source.Status().Reason != "Closed" {
		test.Fatal("closed identity remained usable or Close was not idempotent")
	}
}

func TestWorkloadIdentityRejectsUnprotectedConfiguration(test *testing.T) {
	for _, socket := range []string{"", "tcp://127.0.0.1:12345", "tcp://remote:12345", "https://issuer/socket", "unix:///socket?override=true", `npipe:\\remote\pipe\spire`} {
		if _, err := NewWorkloadIdentity(context.Background(), WorkloadIdentityConfig{SocketAddress: socket, ExpectedID: testIncarnationID, TrustDomain: "kognition.dev"}); err == nil {
			test.Fatalf("accepted unprotected socket %q", socket)
		}
	}
}
