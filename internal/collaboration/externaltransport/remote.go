// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

const (
	agentCardPath = "/.well-known/agent-card.json"
	// maxCardBytes bounds the Agent Card document.
	maxCardBytes = 1 << 20
	callTimeout  = 30 * time.Second
	dialTimeout  = 10 * time.Second
)

var (
	// ErrNotSent marks a failure that happened before any task message was
	// sent, so the agent is known not to have received the task.
	ErrNotSent = errors.New("external agent was not sent the task")
	// ErrCardMismatch reports that the served Agent Card is not the pinned one.
	ErrCardMismatch = errors.New("agent card does not match the pinned digest")

	errNoPinnedInterface = errors.New("agent card declares no interface at the pinned endpoint")
	errRedirect          = errors.New("external agent redirects are refused")
	errResponseTooLarge  = errors.New("external agent response exceeds the size limit")
	errUnpinnedRequest   = errors.New("request to an endpoint other than the pinned one is refused")
)

// RemoteOptions configure RemoteClient.
type RemoteOptions struct {
	// RootCAs verifies the agent certificate; nil uses the system roots.
	RootCAs *x509.CertPool
	// Timeout bounds one remote call; zero takes the default.
	Timeout time.Duration
}

// RemoteClient is the Client that speaks A2A to a real external agent. Every
// call re-fetches the Agent Card over a connection pinned to the addresses the
// Gate vetted, refuses a card that differs from the pinned digest, and only
// then builds an A2A client whose connections can reach nothing else.
type RemoteClient struct {
	roots   *x509.CertPool
	timeout time.Duration
}

var _ Client = (*RemoteClient)(nil)

// NewRemoteClient returns a RemoteClient.
func NewRemoteClient(opts RemoteOptions) *RemoteClient {
	if opts.Timeout <= 0 {
		opts.Timeout = callTimeout
	}
	return &RemoteClient{roots: opts.RootCAs, timeout: opts.Timeout}
}

// Send implements Client. A failure before the message is sent wraps ErrNotSent.
func (c *RemoteClient) Send(ctx context.Context, call Call, messageID, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	client, err := c.connect(ctx, call)
	if err != nil {
		return "", err
	}
	defer client.Destroy() //nolint:errcheck // closing a finished connection

	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	message.ID = messageID
	noHistory := 0
	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: message,
		Config: &a2a.SendMessageConfig{
			AcceptedOutputModes: []string{"text/plain", collaboration.MediaType},
			ReturnImmediately:   true,
			HistoryLength:       &noHistory,
		},
	})
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	task, ok := result.(*a2a.Task)
	if !ok || task.ID == "" {
		return "", errors.New("external agent did not create a task for the message")
	}
	return string(task.ID), nil
}

// GetTask implements Client.
func (c *RemoteClient) GetTask(ctx context.Context, call Call, taskID string) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	client, err := c.connect(ctx, call)
	if err != nil {
		return nil, err
	}
	defer client.Destroy() //nolint:errcheck // closing a finished connection
	noHistory := 0
	task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(taskID), HistoryLength: &noHistory})
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return task, nil
}

// Cancel implements Client.
func (c *RemoteClient) Cancel(ctx context.Context, call Call, taskID string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	client, err := c.connect(ctx, call)
	if err != nil {
		return err
	}
	defer client.Destroy() //nolint:errcheck // closing a finished connection
	if _, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)}); err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	return nil
}

func (c *RemoteClient) connect(ctx context.Context, call Call) (*a2aclient.Client, error) {
	p := pinnedEndpoint{host: call.Host, port: call.Port, addrs: call.Addrs, token: call.Token, maxResponse: call.Limits.MaxResponseBytes, roots: c.roots}
	card, digest, err := p.fetchCard(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	if digest != call.CardDigest {
		return nil, fmt.Errorf("%w: %w (served %s, pinned %s)", ErrNotSent, ErrCardMismatch, digest, call.CardDigest)
	}
	client, err := p.newClient(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	return client, nil
}

// CardObserver fetches the live Agent Card and returns its digest. It applies
// the same pinned-connection rules as task delivery.
type CardObserver struct {
	Resolver externalagent.Resolver
	RootCAs  *x509.CertPool
}

// ObserveCardDigest implements the ExternalAgent reconciler's observer.
func (o CardObserver) ObserveCardDigest(ctx context.Context, agent api.ExternalAgent) (string, error) {
	host, port, err := api.ParseExternalAgentEndpoint(agent.Spec.Endpoint)
	if err != nil {
		return "", err
	}
	addrs, err := externalagent.ResolvePermitted(ctx, o.Resolver, host)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p := pinnedEndpoint{host: host, port: port, addrs: addrs, maxResponse: maxCardBytes, roots: o.RootCAs}
	_, digest, err := p.fetchCard(ctx)
	return digest, err
}

// pinnedEndpoint is the one host:port a call may reach, on the addresses the
// Gate already vetted. DNS is never consulted again.
type pinnedEndpoint struct {
	host        string
	port        int
	addrs       []netip.Addr
	token       Secret
	maxResponse int64
	roots       *x509.CertPool
}

func (p pinnedEndpoint) hostPort() string { return net.JoinHostPort(p.host, strconv.Itoa(p.port)) }

func (p pinnedEndpoint) dial(ctx context.Context) (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	var failures []error
	for _, addr := range p.addrs {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(p.port)))
		if err == nil {
			return conn, nil
		}
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return nil, errors.New("no permitted address to connect to")
	}
	return nil, errors.Join(failures...)
}

func (p pinnedEndpoint) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.host, RootCAs: p.roots}
}

// httpClient never follows a redirect, never uses a proxy and never sends the
// credential to anything but the pinned endpoint.
func (p pinnedEndpoint) httpClient(withToken bool, limit int64) *http.Client {
	base := &http.Transport{
		Proxy:                  nil,
		DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dial(ctx) },
		TLSClientConfig:        p.tlsConfig(),
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    dialTimeout,
		ResponseHeaderTimeout:  callTimeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	guard := &guardTransport{base: base, pinned: p, limit: limit}
	if withToken {
		guard.token = p.token
	}
	return &http.Client{Transport: guard}
}

type guardTransport struct {
	base   http.RoundTripper
	pinned pinnedEndpoint
	token  Secret
	limit  int64
}

func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host, port, ok := urlHostPort(req.URL)
	if !ok || req.URL.Scheme != "https" || req.URL.User != nil || host != g.pinned.host || port != g.pinned.port {
		return nil, errUnpinnedRequest
	}
	out := req.Clone(req.Context())
	if g.token != "" {
		out.Header.Set("Authorization", "Bearer "+g.token.Reveal())
	}
	resp, err := g.base.RoundTrip(out)
	if err != nil {
		return nil, scrub(err, g.token)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close() //nolint:errcheck // refusing the response
		return nil, errRedirect
	}
	if resp.ContentLength > g.limit {
		resp.Body.Close() //nolint:errcheck // refusing the response
		return nil, errResponseTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, left: g.limit}
	return resp, nil
}

// limitedBody fails, rather than truncates, a body that outgrows its limit.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

func urlHostPort(u *url.URL) (string, int, bool) {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", 0, false
	}
	port := 443
	if raw := u.Port(); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		port = n
	}
	return host, port, true
}

// fetchCard returns the served card and the digest of the exact bytes served.
func (p pinnedEndpoint) fetchCard(ctx context.Context) (*a2a.AgentCard, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+p.hostPort()+agentCardPath, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	// The card is fetched without the credential: it is the least that the pin needs.
	resp, err := p.httpClient(false, maxCardBytes).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch agent card: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch agent card: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read agent card: %w", err)
	}
	digest, err := externalagent.CardDigest(raw)
	if err != nil {
		return nil, "", fmt.Errorf("agent card is not canonicalizable: %w", err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		return nil, "", fmt.Errorf("decode agent card: %w", err)
	}
	return &card, digest, nil
}

// owns reports whether iface names the pinned endpoint over a transport that
// keeps TLS on.
func (p pinnedEndpoint) owns(iface *a2a.AgentInterface) bool {
	if iface == nil {
		return false
	}
	raw := strings.TrimSpace(iface.URL)
	var host string
	var port int
	var ok bool
	switch iface.ProtocolBinding {
	case a2a.TransportProtocolJSONRPC, a2a.TransportProtocolHTTPJSON:
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil {
			return false
		}
		host, port, ok = urlHostPort(u)
	case a2a.TransportProtocolGRPC:
		if !strings.Contains(raw, "://") {
			raw = "grpcs://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "grpcs" && u.Scheme != "https") || u.User != nil {
			return false
		}
		host, port, ok = urlHostPort(u)
	}
	return ok && host == p.host && port == p.port
}

// newClient builds an A2A client from the verified card over the pinned endpoint.
func (p pinnedEndpoint) newClient(ctx context.Context, card *a2a.AgentCard) (*a2aclient.Client, error) {
	var interfaces []*a2a.AgentInterface
	for _, iface := range card.SupportedInterfaces {
		if p.owns(iface) {
			interfaces = append(interfaces, iface)
		}
	}
	if len(interfaces) == 0 {
		return nil, errNoPinnedInterface
	}
	pinned := *card
	pinned.SupportedInterfaces = interfaces
	httpClient := p.httpClient(true, p.maxResponse)
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(p.tlsConfig())),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return p.dial(ctx) }),
		grpc.WithPerRPCCredentials(bearerCredentials{token: p.token}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(p.maxResponse))),
	}
	return a2aclient.NewFromCard(ctx, &pinned,
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(httpClient),
		a2aclient.WithRESTTransport(httpClient),
		a2aclient.WithTransport(a2a.TransportProtocolGRPC, a2aclient.TransportFactoryFn(
			func(_ context.Context, _ *a2a.AgentCard, _ *a2a.AgentInterface) (a2aclient.Transport, error) {
				// passthrough: the pinned addresses are dialed, never resolved again.
				conn, err := grpc.NewClient("passthrough:///"+p.hostPort(), dialOptions...)
				if err != nil {
					return nil, err
				}
				return a2agrpc.NewGRPCTransport(conn), nil
			})),
	)
}

// bearerCredentials attaches the token to RPCs on the pinned gRPC connection only.
type bearerCredentials struct{ token Secret }

func (b bearerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if b.token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + b.token.Reveal()}, nil
}

func (bearerCredentials) RequireTransportSecurity() bool { return true }
