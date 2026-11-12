// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type workloadPeerKey struct{}

func RequireAgentWorkloadPeer(ctx context.Context, agent *v1alpha1.Agent) error {
	peer, active := ctx.Value(workloadPeerKey{}).(spiffeid.ID)
	if !active {
		return nil
	}
	if agent == nil {
		return errors.New("workload peer requires a live Agent")
	}
	expected, err := AgentIncarnationSPIFFEID(peer.TrustDomain().String(), agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, agent.ObjectMeta.UID)
	if err != nil || peer.String() != expected {
		return errors.New("workload peer does not match the Agent incarnation")
	}
	return nil
}

func ServeGatewayHTTP(ctx context.Context, server *http.Server) error {
	switch os.Getenv("KOGNITION_WORKLOAD_IDENTITY_MODE") {
	case "", "capability":
		return server.ListenAndServe()
	case "spire":
	default:
		return errors.New("unsupported workload identity profile")
	}
	source, err := NewWorkloadIdentity(ctx, WorkloadIdentityConfig{
		SocketAddress: os.Getenv("KOGNITION_SPIFFE_ENDPOINT_SOCKET"),
		ExpectedID:    os.Getenv("KOGNITION_SPIFFE_ID"), TrustDomain: os.Getenv("KOGNITION_SPIFFE_TRUST_DOMAIN"),
	})
	if err != nil {
		return ErrWorkloadIdentityUnavailable
	}
	defer func() { _ = source.Close() }()
	domain := source.expected.TrustDomain()
	server.TLSConfig = tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeMemberOf(domain))
	server.TLSConfig.MinVersion = tls.VersionTLS13
	handler := server.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	server.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !source.Status().Ready || request.TLS == nil {
			http.Error(writer, "workload identity unavailable", http.StatusServiceUnavailable)
			return
		}
		peer, _, err := x509svid.Verify(request.TLS.PeerCertificates, source)
		if err != nil || peer.TrustDomain() != domain {
			http.Error(writer, "workload peer not authorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), workloadPeerKey{}, peer)))
	})
	return server.ListenAndServeTLS("", "")
}
