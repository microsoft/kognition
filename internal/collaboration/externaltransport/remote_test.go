// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/grpc"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

const remoteHost = "agent.example.test"

// scriptedExecutor completes immediately, or holds the task open until cancelled.
type scriptedExecutor struct {
	hold bool
	stop chan struct{}
}

func (e *scriptedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		if e.hold {
			if yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				select {
				case <-ctx.Done():
				case <-e.stop:
				}
			}
			return
		}
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("remote answer")), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *scriptedExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

type remoteOpts struct {
	hold         bool
	withGRPC     bool
	interfaceURL string // overrides the JSON-RPC interface URL
	cardHandler  func(w http.ResponseWriter, r *http.Request) bool
}

type remoteServer struct {
	t     *testing.T
	srv   *httptest.Server
	port  int
	roots *x509.CertPool
	opts  remoteOpts

	mu        sync.Mutex
	extra     string
	cardAuth  []string
	rpcAuth   []string
	rpcCalls  int
	hitOthers int
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: remoteHost},
		DNSNames:              []string{remoteHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func startRemote(t *testing.T, opts remoteOpts) *remoteServer {
	t.Helper()
	cert, pool := selfSignedCert(t)
	rs := &remoteServer{t: t, roots: pool, opts: opts}
	executor := &scriptedExecutor{hold: opts.hold, stop: make(chan struct{})}
	t.Cleanup(func() { close(executor.stop) })
	handler := a2asrv.NewHandler(executor)
	rpc := a2asrv.NewJSONRPCHandler(handler)
	grpcServer := grpc.NewServer()
	a2agrpc.NewHandler(handler).RegisterWith(grpcServer)

	mux := http.NewServeMux()
	mux.HandleFunc(agentCardPath, func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.cardAuth = append(rs.cardAuth, r.Header.Get("Authorization"))
		rs.mu.Unlock()
		if opts.cardHandler != nil && opts.cardHandler(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rs.cardBytes())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			rs.mu.Lock()
			rs.rpcAuth = append(rs.rpcAuth, r.Header.Get("Authorization"))
			rs.rpcCalls++
			rs.mu.Unlock()
			grpcServer.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			rs.mu.Lock()
			rs.hitOthers++
			rs.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		rs.mu.Lock()
		rs.rpcAuth = append(rs.rpcAuth, r.Header.Get("Authorization"))
		rs.rpcCalls++
		rs.mu.Unlock()
		rpc.ServeHTTP(w, r)
	})
	rs.srv = httptest.NewUnstartedServer(mux)
	rs.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	rs.srv.EnableHTTP2 = true
	rs.srv.StartTLS()
	t.Cleanup(rs.srv.Close)
	_, portText, err := net.SplitHostPort(rs.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rs.port, _ = strconv.Atoi(portText)
	return rs
}

func (rs *remoteServer) cardBytes() []byte {
	rs.mu.Lock()
	extra := rs.extra
	rs.mu.Unlock()
	hostPort := net.JoinHostPort(remoteHost, strconv.Itoa(rs.port))
	rpcURL := "https://" + hostPort + "/"
	if rs.opts.interfaceURL != "" {
		rpcURL = rs.opts.interfaceURL
	}
	interfaces := []*a2a.AgentInterface{a2a.NewAgentInterface(rpcURL, a2a.TransportProtocolJSONRPC)}
	if rs.opts.withGRPC {
		interfaces = []*a2a.AgentInterface{a2a.NewAgentInterface(hostPort, a2a.TransportProtocolGRPC)}
	}
	card := a2a.AgentCard{
		Name:                "remote",
		Description:         "scripted external agent " + extra,
		Version:             "1",
		SupportedInterfaces: interfaces,
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{{ID: "s", Name: "s", Description: "s", Tags: []string{"t"}}},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		rs.t.Fatal(err)
	}
	return raw
}

func (rs *remoteServer) digest() string {
	rs.t.Helper()
	digest, err := externalagent.CardDigest(rs.cardBytes())
	if err != nil {
		rs.t.Fatal(err)
	}
	return digest
}

func (rs *remoteServer) call() Call {
	limits := api.ExternalAgentLimits{}
	return Call{
		Namespace:  testNS,
		Agent:      "remote",
		Host:       remoteHost,
		Port:       rs.port,
		Addrs:      []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		CardDigest: rs.digest(),
		Token:      Secret(testToken),
		Limits:     limits.Effective(),
	}
}

func (rs *remoteServer) client() *RemoteClient {
	return NewRemoteClient(RemoteOptions{RootCAs: rs.roots, Timeout: 10 * time.Second})
}

func (rs *remoteServer) rpcSnapshot() (int, []string, []string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.rpcCalls, append([]string(nil), rs.rpcAuth...), append([]string(nil), rs.cardAuth...)
}

func TestRemoteClientRoundTripOverJSONRPC(t *testing.T) { // TC-0355
	rs := startRemote(t, remoteOpts{})
	client, call := rs.client(), rs.call()
	taskID, err := client.Send(context.Background(), call, "dispatch-1", "do the thing")
	if err != nil || taskID == "" {
		t.Fatalf("Send = %q %v", taskID, err)
	}
	task, err := client.GetTask(context.Background(), call, taskID)
	if err != nil || string(task.ID) != taskID {
		t.Fatalf("GetTask = %+v %v", task, err)
	}
	calls, rpcAuth, cardAuth := rs.rpcSnapshot()
	if calls < 2 {
		t.Fatalf("rpc calls = %d, want send and get", calls)
	}
	for _, header := range rpcAuth {
		if header != "Bearer "+testToken {
			t.Fatalf("task call carried Authorization %q", header)
		}
	}
	for _, header := range cardAuth {
		if header != "" {
			t.Fatalf("card fetch carried a credential: %q", header)
		}
	}
}

func TestRemoteClientCancelsARunningTask(t *testing.T) { // TC-0355
	rs := startRemote(t, remoteOpts{hold: true})
	client, call := rs.client(), rs.call()
	taskID, err := client.Send(context.Background(), call, "dispatch-2", "long job")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Cancel(context.Background(), call, taskID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	task, err := client.GetTask(context.Background(), call, taskID)
	if err != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("after cancel: %+v %v", task, err)
	}
}

func TestRemoteClientRoundTripOverGRPC(t *testing.T) { // TC-0356
	rs := startRemote(t, remoteOpts{withGRPC: true})
	client, call := rs.client(), rs.call()
	taskID, err := client.Send(context.Background(), call, "dispatch-3", "over grpc")
	if err != nil || taskID == "" {
		t.Fatalf("Send = %q %v", taskID, err)
	}
	if _, err := client.GetTask(context.Background(), call, taskID); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	_, rpcAuth, _ := rs.rpcSnapshot()
	if len(rpcAuth) == 0 {
		t.Fatal("no gRPC call reached the server")
	}
	for _, header := range rpcAuth {
		if header != "Bearer "+testToken {
			t.Fatalf("gRPC call carried Authorization %q", header)
		}
	}
}

func TestRemoteClientRefusesAChangedCard(t *testing.T) { // TC-0357
	rs := startRemote(t, remoteOpts{})
	client, call := rs.client(), rs.call()
	rs.mu.Lock()
	rs.extra = "now different"
	rs.mu.Unlock()
	_, err := client.Send(context.Background(), call, "dispatch-4", "hello")
	if !errors.Is(err, ErrCardMismatch) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("Send error = %v, want card mismatch and not sent", err)
	}
	if calls, _, _ := rs.rpcSnapshot(); calls != 0 {
		t.Fatalf("a task call reached an agent whose card changed: %d", calls)
	}
}

func TestRemoteClientRefusesRedirectsAndNeverFollowsThem(t *testing.T) { // TC-0358
	redirect := func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
		return true
	}
	rs := startRemote(t, remoteOpts{cardHandler: redirect})
	call := rs.call()
	call.CardDigest = "sha256:" + strings.Repeat("0", 64)
	_, err := rs.client().Send(context.Background(), call, "dispatch-5", "hello")
	if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), errRedirect.Error()) {
		t.Fatalf("Send error = %v, want a refused redirect", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.hitOthers != 0 {
		t.Fatalf("redirect target was requested %d times", rs.hitOthers)
	}
}

func TestRemoteClientBoundsTheCardSize(t *testing.T) { // TC-0358
	huge := func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":%q}`, strings.Repeat("a", maxCardBytes+1024))
		return true
	}
	rs := startRemote(t, remoteOpts{cardHandler: huge})
	_, err := rs.client().Send(context.Background(), rs.call(), "dispatch-6", "hello")
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("Send error = %v, want not sent", err)
	}
}

func TestRemoteClientRefusesAnInterfaceAtAnotherEndpoint(t *testing.T) { // TC-0359
	rs := startRemote(t, remoteOpts{interfaceURL: "https://other.example.test/"})
	_, err := rs.client().Send(context.Background(), rs.call(), "dispatch-7", "hello")
	if !errors.Is(err, errNoPinnedInterface) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("Send error = %v, want no pinned interface", err)
	}
	if calls, _, _ := rs.rpcSnapshot(); calls != 0 {
		t.Fatalf("a task call was made: %d", calls)
	}
}

func TestRemoteClientRejectsAnUntrustedCertificate(t *testing.T) { // TC-0359
	rs := startRemote(t, remoteOpts{})
	client := NewRemoteClient(RemoteOptions{RootCAs: x509.NewCertPool(), Timeout: 10 * time.Second})
	_, err := client.Send(context.Background(), rs.call(), "dispatch-8", "hello")
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("Send error = %v, want not sent", err)
	}
	if calls, _, _ := rs.rpcSnapshot(); calls != 0 {
		t.Fatalf("a task call was made over an untrusted certificate: %d", calls)
	}
}

func TestRemoteClientOnlyDialsThePinnedAddresses(t *testing.T) { // TC-0359
	rs := startRemote(t, remoteOpts{})
	call := rs.call()
	call.Addrs = nil
	_, err := rs.client().Send(context.Background(), call, "dispatch-9", "hello")
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("Send error = %v, want not sent", err)
	}
}

func TestPinnedEndpointFetchCardDigestMatchesTheServedBytes(t *testing.T) { // TC-0360
	rs := startRemote(t, remoteOpts{})
	p := pinnedEndpoint{host: remoteHost, port: rs.port, addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, maxResponse: maxCardBytes, roots: rs.roots}
	card, digest, err := p.fetchCard(context.Background())
	if err != nil || card == nil || digest != rs.digest() {
		t.Fatalf("fetchCard = %v %q %v, want digest %q", card, digest, err, rs.digest())
	}
}

func TestCardObserverRefusesForbiddenAddressesWithoutConnecting(t *testing.T) { // TC-0360
	rs := startRemote(t, remoteOpts{})
	observer := CardObserver{
		Resolver: &stubResolver{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		RootCAs:  rs.roots,
	}
	agent := api.ExternalAgent{Spec: api.ExternalAgentSpec{Endpoint: "https://" + net.JoinHostPort(remoteHost, strconv.Itoa(rs.port))}}
	if digest, err := observer.ObserveCardDigest(context.Background(), agent); err == nil || digest != "" {
		t.Fatalf("ObserveCardDigest = %q %v, want a refusal", digest, err)
	}
	if _, _, cardAuth := rs.rpcSnapshot(); len(cardAuth) != 0 {
		t.Fatalf("the card endpoint was contacted %d times", len(cardAuth))
	}
}

func TestPreSendFailureSettlesAsNotSent(t *testing.T) { // TC-0357
	f := newFixture(t)
	f.client.sendErr = fmt.Errorf("%w: card changed", ErrNotSent)
	tr := f.transport()
	msg := startMessage("d-notsent")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), msg); err == nil {
		t.Fatal("expected a send error")
	}
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportFailed || obs.Failure != FailureNotSent {
		t.Fatalf("Observe = %+v %v, want settled NotSent without waiting for the deadline", obs, err)
	}
}
