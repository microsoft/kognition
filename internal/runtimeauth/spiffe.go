// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"k8s.io/apimachinery/pkg/util/validation"
)

var ErrWorkloadIdentityUnavailable = errors.New("workload identity unavailable")

type WorkloadIdentityConfig struct {
	SocketAddress string
	ExpectedID    string
	TrustDomain   string
}

type WorkloadIdentityStatus struct {
	SPIFFEID    string
	TrustDomain string
	NotBefore   time.Time
	ExpiresAt   time.Time
	Generation  uint64
	Ready       bool
	Reason      string
}

type WorkloadIdentity struct {
	mu       sync.RWMutex
	expected spiffeid.ID
	client   *workloadapi.Client
	cancel   context.CancelFunc
	done     chan struct{}
	initial  chan struct{}
	close    sync.Once
	closed   bool
	material *workloadapi.X509Context
	svid     *x509svid.SVID
	status   WorkloadIdentityStatus
}

func AgentIncarnationSPIFFEID(trustDomain, namespace, agentName, agentUID string) (string, error) {
	domain, err := spiffeid.TrustDomainFromString(trustDomain)
	parsedUID, uidErr := uuid.Parse(agentUID)
	if err != nil || domain.String() != trustDomain || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(agentName)) != 0 || uidErr != nil || parsedUID == uuid.Nil || parsedUID.String() != agentUID {
		return "", errors.New("invalid Agent incarnation identity")
	}
	id, err := spiffeid.FromPath(domain, fmt.Sprintf("/workspace/%s/agent/%s/incarnation/%s", namespace, agentName, agentUID))
	if err != nil {
		return "", errors.New("invalid Agent incarnation identity")
	}
	return id.String(), nil
}

func NewWorkloadIdentity(ctx context.Context, config WorkloadIdentityConfig) (*WorkloadIdentity, error) {
	endpoint, err := url.Parse(config.SocketAddress)
	if err != nil || endpoint == nil || (endpoint.Scheme != "unix" && endpoint.Scheme != "npipe") || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return nil, errors.New("a protected local Workload API socket is required")
	}
	if endpoint.Scheme == "npipe" && !strings.HasPrefix(endpoint.Opaque, `\\.\pipe\`) {
		return nil, errors.New("a local Workload API named pipe is required")
	}
	if err := workloadapi.ValidateAddress(config.SocketAddress); err != nil {
		return nil, errors.New("invalid Workload API socket")
	}
	expected, err := spiffeid.FromString(config.ExpectedID)
	if err != nil || expected.Path() == "" || expected.TrustDomain().String() != config.TrustDomain {
		return nil, errors.New("expected workload identity does not match the trust domain")
	}
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(config.SocketAddress))
	if err != nil {
		return nil, ErrWorkloadIdentityUnavailable
	}
	return startWorkloadIdentity(ctx, expected, client)
}

func startWorkloadIdentity(ctx context.Context, expected spiffeid.ID, client *workloadapi.Client) (*WorkloadIdentity, error) {
	watchContext, cancel := context.WithCancel(ctx)
	source := &WorkloadIdentity{
		expected: expected, client: client, cancel: cancel,
		done: make(chan struct{}), initial: make(chan struct{}, 1),
		status: WorkloadIdentityStatus{SPIFFEID: expected.String(), TrustDomain: expected.TrustDomain().String(), Reason: "AwaitingIdentity"},
	}
	go func() {
		defer close(source.done)
		_ = client.WatchX509Context(watchContext, source)
		source.OnX509ContextWatchError(ErrWorkloadIdentityUnavailable)
	}()
	initialContext, initialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initialCancel()
	select {
	case <-source.initial:
		if source.Status().Ready {
			return source, nil
		}
	case <-initialContext.Done():
	}
	_ = source.Close()
	return nil, ErrWorkloadIdentityUnavailable
}

func (source *WorkloadIdentity) OnX509ContextUpdate(material *workloadapi.X509Context) {
	var selected *x509svid.SVID
	valid := material != nil && material.Bundles != nil
	if valid {
		for _, candidate := range material.SVIDs {
			if candidate != nil && candidate.ID == source.expected {
				if selected != nil {
					valid = false
					break
				}
				selected = candidate
			}
		}
		valid = valid && selected != nil && len(selected.Certificates) > 0 && selected.PrivateKey != nil
	}
	if valid {
		verified, _, err := x509svid.Verify(selected.Certificates, material.Bundles)
		valid = err == nil && verified == source.expected
		if valid {
			certificateKey, certificateErr := x509.MarshalPKIXPublicKey(selected.Certificates[0].PublicKey)
			privateKey, privateErr := x509.MarshalPKIXPublicKey(selected.PrivateKey.Public())
			valid = certificateErr == nil && privateErr == nil && bytes.Equal(certificateKey, privateKey)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed {
		return
	}
	source.material, source.svid = nil, nil
	source.status.Ready = valid
	source.status.Reason = "InvalidIdentity"
	if valid {
		source.material, source.svid = material, selected
		source.status.NotBefore = selected.Certificates[0].NotBefore.UTC()
		source.status.ExpiresAt = selected.Certificates[0].NotAfter.UTC()
		source.status.Generation++
		source.status.Reason = "Ready"
	}
	select {
	case source.initial <- struct{}{}:
	default:
	}
}

func (source *WorkloadIdentity) OnX509ContextWatchError(_ error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.material, source.svid = nil, nil
	source.status.Ready = false
	if !source.closed {
		source.status.Reason = "IssuerUnavailable"
	}
	select {
	case source.initial <- struct{}{}:
	default:
	}
}

func (source *WorkloadIdentity) statusLocked() WorkloadIdentityStatus {
	status := source.status
	if status.Ready && (source.closed || time.Now().Before(status.NotBefore) || !time.Now().Before(status.ExpiresAt)) {
		status.Ready = false
		status.Reason = "ExpiredIdentity"
	}
	return status
}

func (source *WorkloadIdentity) Status() WorkloadIdentityStatus {
	source.mu.RLock()
	defer source.mu.RUnlock()
	return source.statusLocked()
}

func (source *WorkloadIdentity) AgentStatus(namespace, agentName, agentUID string) (WorkloadIdentityStatus, error) {
	if source == nil {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	expected, err := AgentIncarnationSPIFFEID(source.expected.TrustDomain().String(), namespace, agentName, agentUID)
	if err != nil || expected != source.expected.String() {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	return source.Status(), nil
}

func (source *WorkloadIdentity) GetX509SVID() (*x509svid.SVID, error) {
	source.mu.RLock()
	defer source.mu.RUnlock()
	if !source.statusLocked().Ready {
		return nil, ErrWorkloadIdentityUnavailable
	}
	return source.svid, nil
}

func (source *WorkloadIdentity) GetX509BundleForTrustDomain(domain spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	source.mu.RLock()
	defer source.mu.RUnlock()
	if !source.statusLocked().Ready || domain != source.expected.TrustDomain() {
		return nil, ErrWorkloadIdentityUnavailable
	}
	return source.material.Bundles.GetX509BundleForTrustDomain(domain)
}

func (source *WorkloadIdentity) HTTPClient(peerIdentity string) (*http.Client, error) {
	peer, err := spiffeid.FromString(peerIdentity)
	if err != nil || peer.Path() == "" || peer.TrustDomain() != source.expected.TrustDomain() {
		return nil, errors.New("invalid peer workload identity")
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return nil, errors.New("default HTTP transport is unavailable")
	}
	transport := defaultTransport.Clone()
	transport.DisableKeepAlives = true
	transport.TLSClientConfig = tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeID(peer))
	transport.TLSClientConfig.MinVersion = tls.VersionTLS13
	return &http.Client{
		Transport:     &workloadTransport{source: source, transport: transport},
		Timeout:       30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type workloadTransport struct {
	source    *WorkloadIdentity
	transport *http.Transport
}

func (transport *workloadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || !transport.source.Status().Ready {
		return nil, ErrWorkloadIdentityUnavailable
	}
	return transport.transport.RoundTrip(request)
}

func (transport *workloadTransport) CloseIdleConnections() {
	transport.transport.CloseIdleConnections()
}

func (source *WorkloadIdentity) Close() error {
	source.close.Do(func() {
		source.mu.Lock()
		source.closed = true
		source.material, source.svid = nil, nil
		source.status.Ready = false
		source.status.Reason = "Closed"
		source.mu.Unlock()
		source.cancel()
		_ = source.client.Close()
		<-source.done
	})
	return nil
}
