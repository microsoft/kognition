// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pluginsdk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type KnowledgeTLSConfig struct {
	Certificate tls.Certificate
	Roots       *x509.CertPool
	PeerURI     string
	ServerName  string
}

func knowledgeTLS(config KnowledgeTLSConfig, server bool) (*tls.Config, error) {
	identity, err := url.Parse(config.PeerURI)
	if err != nil || identity.Scheme != "spiffe" || identity.Host == "" || identity.Path == "" || identity.User != nil || identity.RawQuery != "" || identity.Fragment != "" {
		return nil, fmt.Errorf("knowledge transport requires an exact SPIFFE peer URI")
	}
	if config.Roots == nil || len(config.Certificate.Certificate) == 0 || config.Certificate.PrivateKey == nil || (!server && config.ServerName == "") {
		return nil, fmt.Errorf("knowledge transport requires a certificate, trust roots and client-side server name")
	}
	transport := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{config.Certificate},
		ServerName:   config.ServerName,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return fmt.Errorf("knowledge peer certificate is not verified")
			}
			identities := state.PeerCertificates[0].URIs
			if len(identities) != 1 || identities[0].String() != config.PeerURI {
				return fmt.Errorf("knowledge peer identity does not match registration")
			}
			return nil
		},
	}
	if server {
		transport.ClientAuth = tls.RequireAndVerifyClientCert
		transport.ClientCAs = config.Roots.Clone()
	} else {
		transport.RootCAs = config.Roots.Clone()
	}
	return transport, nil
}

type KnowledgeServerConfig struct {
	Name                string
	Version             string
	TLS                 KnowledgeTLSConfig
	Capabilities        *protocol.KnowledgeProviderCapabilities
	AuthorizeQueryScope func(context.Context, *protocol.KnowledgeScope) error
	AuthorizeIndexScope func(context.Context, *protocol.KnowledgeScope) error
	AuthorizeCleanup    func(context.Context, *protocol.KnowledgeCleanupRequest) error
	Probe               func(context.Context) error
	Querier             KnowledgeQuerier
	Indexer             KnowledgeIndexer
	Cleaner             KnowledgeCleaner
}

type KnowledgeIndexer interface {
	BeginIndex(context.Context, *protocol.KnowledgeScope, string, string) (KnowledgeIndexTransaction, error)
}

type KnowledgeIndexTransaction interface {
	WriteBatch(context.Context, *protocol.KnowledgeIndexBatchRequest) error
	Commit(context.Context) (*protocol.KnowledgeIndexResponse, error)
	Abort(context.Context) error
}

type KnowledgeCleaner interface {
	Cleanup(context.Context, *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error)
}

type knowledgeServer struct {
	protocol.UnimplementedIdentityServer
	protocol.UnimplementedLifecycleServer
	protocol.UnimplementedKnowledgeExtensionServer
	config KnowledgeServerConfig
}

func NewKnowledgeServer(config KnowledgeServerConfig) (*grpc.Server, error) {
	if strings.TrimSpace(config.Name) == "" || config.Version == "" || config.AuthorizeQueryScope == nil || config.Probe == nil || config.Querier == nil || config.Capabilities == nil ||
		config.Capabilities.ProtocolVersion != KnowledgeProtocolVersion || len(config.Capabilities.ConfigurationKinds) == 0 {
		return nil, fmt.Errorf("knowledge server requires identity, capabilities, scope authorization, probe and querier")
	}
	if slices.Contains(config.Capabilities.Operations, "index") && (config.Indexer == nil || config.AuthorizeIndexScope == nil) {
		return nil, fmt.Errorf("knowledge server advertising index requires an indexer and index scope authorization")
	}
	if (slices.Contains(config.Capabilities.Operations, "cleanup-delete") || slices.Contains(config.Capabilities.Operations, "cleanup-retain") || slices.Contains(config.Capabilities.Operations, "cleanup-archive")) &&
		(config.Cleaner == nil || config.AuthorizeCleanup == nil) {
		return nil, fmt.Errorf("knowledge server advertising cleanup requires a cleaner and cleanup authorization")
	}
	transport, err := knowledgeTLS(config.TLS, true)
	if err != nil {
		return nil, err
	}
	capabilities, ok := proto.Clone(config.Capabilities).(*protocol.KnowledgeProviderCapabilities)
	if !ok {
		return nil, fmt.Errorf("clone knowledge provider capabilities")
	}
	config.Capabilities = capabilities
	implementation := &knowledgeServer{config: config}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(transport)), grpc.MaxRecvMsgSize(knowledgeMessageLimit), grpc.MaxSendMsgSize(knowledgeMessageLimit))
	protocol.RegisterIdentityServer(server, implementation)
	protocol.RegisterLifecycleServer(server, implementation)
	protocol.RegisterKnowledgeExtensionServer(server, implementation)
	return server, nil
}

func (server *knowledgeServer) GetProviderInfo(context.Context, *protocol.GetProviderInfoRequest) (*protocol.GetProviderInfoResponse, error) {
	return &protocol.GetProviderInfoResponse{Name: server.config.Name, Version: server.config.Version, Dimension: "knowledge"}, nil
}

func (server *knowledgeServer) GetProviderCapabilities(context.Context, *protocol.GetProviderCapabilitiesRequest) (*protocol.GetProviderCapabilitiesResponse, error) {
	capabilities, ok := proto.Clone(server.config.Capabilities).(*protocol.KnowledgeProviderCapabilities)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge provider capabilities")
	}
	return &protocol.GetProviderCapabilitiesResponse{Knowledge: capabilities}, nil
}

func (server *knowledgeServer) HealthCheck(ctx context.Context, _ *protocol.HealthCheckRequest) (*protocol.HealthCheckResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	if err := server.config.Probe(ctx); err != nil {
		return nil, knowledgeError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return &protocol.HealthCheckResponse{Ready: true}, nil
}

func (server *knowledgeServer) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if err := ValidateKnowledgeQuery(request); err != nil {
		return nil, err
	}
	if err := validateKnowledgeCapabilities(server.config.Capabilities, request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	authorizationScope, ok := proto.Clone(request.Scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge query scope")
	}
	if err := server.config.AuthorizeQueryScope(ctx, authorizationScope); err != nil {
		return nil, knowledgeError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	queryRequest, ok := proto.Clone(request).(*protocol.KnowledgeQueryRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge query request")
	}
	response, err := server.config.Querier.Query(ctx, queryRequest)
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := validateKnowledgeResponse(request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (server *knowledgeServer) Index(stream grpc.ClientStreamingServer[protocol.KnowledgeIndexBatchRequest, protocol.KnowledgeIndexResponse]) error {
	if server.config.Indexer == nil || !slices.Contains(server.config.Capabilities.Operations, "index") {
		return status.Error(codes.FailedPrecondition, "knowledge provider does not support indexing")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Minute)
	defer cancel()
	var transaction KnowledgeIndexTransaction
	var scope *protocol.KnowledgeScope
	var operationID, sourceDigest string
	documents := make(map[string]struct{})
	var chunkCount int64
	sequence := int32(0)
	committed := false
	defer func() {
		if transaction != nil && !committed {
			abortCtx, abortCancel := context.WithTimeout(context.WithoutCancel(stream.Context()), knowledgeTimeout)
			defer abortCancel()
			_ = transaction.Abort(abortCtx)
		}
	}()
	for {
		request, err := receiveKnowledgeIndexBatch(ctx, stream)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return knowledgeError(err)
		}
		if err := ValidateKnowledgeIndexBatch(request); err != nil {
			return err
		}
		if transaction == nil {
			if request.Sequence != 0 {
				return status.Error(codes.InvalidArgument, "knowledge index stream must start at sequence zero")
			}
			if err := validateKnowledgeIndexCapabilities(server.config.Capabilities, request.Scope); err != nil {
				return err
			}
			authorizationScope, ok := proto.Clone(request.Scope).(*protocol.KnowledgeScope)
			if !ok {
				return status.Error(codes.Internal, "clone knowledge index scope")
			}
			if err := server.config.AuthorizeIndexScope(ctx, authorizationScope); err != nil {
				return knowledgeError(err)
			}
			scope, ok = proto.Clone(request.Scope).(*protocol.KnowledgeScope)
			if !ok {
				return status.Error(codes.Internal, "clone knowledge index scope")
			}
			operationID, sourceDigest = request.OperationId, request.SourceDigest
			indexScope, ok := proto.Clone(scope).(*protocol.KnowledgeScope)
			if !ok {
				return status.Error(codes.Internal, "clone knowledge index scope")
			}
			transaction, err = server.config.Indexer.BeginIndex(ctx, indexScope, operationID, sourceDigest)
			if err != nil {
				return knowledgeError(err)
			}
		} else if request.Sequence != sequence || request.OperationId != operationID || request.SourceDigest != sourceDigest || !proto.Equal(request.Scope, scope) {
			return status.Error(codes.InvalidArgument, "knowledge index stream binding mismatch")
		}
		batchRequest, ok := proto.Clone(request).(*protocol.KnowledgeIndexBatchRequest)
		if !ok {
			return status.Error(codes.Internal, "clone knowledge index batch")
		}
		if err := transaction.WriteBatch(ctx, batchRequest); err != nil {
			return knowledgeError(err)
		}
		for _, chunk := range request.Chunks {
			documents[chunk.DocumentId] = struct{}{}
			chunkCount++
		}
		sequence++
	}
	if transaction == nil {
		return status.Error(codes.InvalidArgument, "knowledge index stream is empty")
	}
	response, err := transaction.Commit(ctx)
	if err != nil {
		return knowledgeError(err)
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if err := validateKnowledgeIndexResponse(scope, operationID, sourceDigest, int64(len(documents)), chunkCount, response); err != nil {
		return err
	}
	committed = true
	return stream.SendAndClose(response)
}

func receiveKnowledgeIndexBatch(ctx context.Context, stream grpc.ClientStreamingServer[protocol.KnowledgeIndexBatchRequest, protocol.KnowledgeIndexResponse]) (*protocol.KnowledgeIndexBatchRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type receiveResult struct {
		request *protocol.KnowledgeIndexBatchRequest
		err     error
	}
	result := make(chan receiveResult, 1)
	go func() {
		request, err := stream.Recv()
		result <- receiveResult{request: request, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case received := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return received.request, received.err
	}
}

func (server *knowledgeServer) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if err := ValidateKnowledgeCleanup(request); err != nil {
		return nil, err
	}
	if err := validateKnowledgeCleanupCapabilities(server.config.Capabilities, request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	authorizationRequest, ok := proto.Clone(request).(*protocol.KnowledgeCleanupRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge cleanup request")
	}
	if err := server.config.AuthorizeCleanup(ctx, authorizationRequest); err != nil {
		return nil, knowledgeError(err)
	}
	cleanupRequest, ok := proto.Clone(request).(*protocol.KnowledgeCleanupRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge cleanup request")
	}
	response, err := server.config.Cleaner.Cleanup(ctx, cleanupRequest)
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := validateKnowledgeCleanupResponse(request, response); err != nil {
		return nil, err
	}
	return response, nil
}

type KnowledgeClientConfig struct {
	Name    string
	Address string
	TLS     KnowledgeTLSConfig
}

type KnowledgeClient struct {
	connection *grpc.ClientConn
	name       string
}

func NewKnowledgeClient(config KnowledgeClientConfig) (*KnowledgeClient, error) {
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(config.Address, "/?#") || strings.TrimSpace(config.Name) == "" {
		return nil, fmt.Errorf("knowledge client requires a registered provider name and host:port address")
	}
	transport, err := knowledgeTLS(config.TLS, false)
	if err != nil {
		return nil, err
	}
	connection, err := grpc.NewClient("passthrough:///"+config.Address, grpc.WithTransportCredentials(credentials.NewTLS(transport)), grpc.WithDisableRetry(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(knowledgeMessageLimit), grpc.MaxCallSendMsgSize(knowledgeMessageLimit)))
	if err != nil {
		return nil, err
	}
	return &KnowledgeClient{connection: connection, name: config.Name}, nil
}

func (client *KnowledgeClient) Close() error {
	return client.connection.Close()
}

func (client *KnowledgeClient) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if err := client.PreflightQuery(ctx, request); err != nil {
		return nil, err
	}
	request, ok := proto.Clone(request).(*protocol.KnowledgeQueryRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge query request")
	}
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	response, err := protocol.NewKnowledgeExtensionClient(client.connection).Query(ctx, request)
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := validateKnowledgeResponse(request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (client *KnowledgeClient) PreflightQuery(ctx context.Context, request *protocol.KnowledgeQueryRequest) error {
	if err := ValidateKnowledgeQuery(request); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	identity := protocol.NewIdentityClient(client.connection)
	info, err := identity.GetProviderInfo(ctx, &protocol.GetProviderInfoRequest{})
	if err != nil {
		return knowledgeError(err)
	}
	if info.Name != client.name || info.Dimension != "knowledge" {
		return status.Error(codes.FailedPrecondition, "knowledge provider identity mismatch")
	}
	capabilities, err := identity.GetProviderCapabilities(ctx, &protocol.GetProviderCapabilitiesRequest{})
	if err != nil {
		return knowledgeError(err)
	}
	if err := validateKnowledgeCapabilities(capabilities.Knowledge, request); err != nil {
		return err
	}
	return nil
}

func (client *KnowledgeClient) Index(ctx context.Context, batches []*protocol.KnowledgeIndexBatchRequest) (*protocol.KnowledgeIndexResponse, error) {
	if len(batches) == 0 {
		return nil, status.Error(codes.InvalidArgument, "knowledge index stream is empty")
	}
	first := batches[0]
	documents := make(map[string]struct{})
	var chunkCount int64
	for index, batch := range batches {
		if err := ValidateKnowledgeIndexBatch(batch); err != nil {
			return nil, err
		}
		if batch.Sequence != int32(index) || batch.OperationId != first.OperationId || batch.SourceDigest != first.SourceDigest || !proto.Equal(batch.Scope, first.Scope) {
			return nil, status.Error(codes.InvalidArgument, "knowledge index stream binding mismatch")
		}
		for _, chunk := range batch.Chunks {
			documents[chunk.DocumentId] = struct{}{}
			chunkCount++
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	identity := protocol.NewIdentityClient(client.connection)
	info, err := identity.GetProviderInfo(ctx, &protocol.GetProviderInfoRequest{})
	if err != nil {
		return nil, knowledgeError(err)
	}
	if info.Name != client.name || info.Dimension != "knowledge" {
		return nil, status.Error(codes.FailedPrecondition, "knowledge provider identity mismatch")
	}
	capabilities, err := identity.GetProviderCapabilities(ctx, &protocol.GetProviderCapabilitiesRequest{})
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := validateKnowledgeIndexCapabilities(capabilities.Knowledge, first.Scope); err != nil {
		return nil, err
	}
	stream, err := protocol.NewKnowledgeExtensionClient(client.connection).Index(ctx)
	if err != nil {
		return nil, knowledgeError(err)
	}
	for _, batch := range batches {
		batchRequest, ok := proto.Clone(batch).(*protocol.KnowledgeIndexBatchRequest)
		if !ok {
			return nil, status.Error(codes.Internal, "clone knowledge index batch")
		}
		if err := stream.Send(batchRequest); err != nil {
			return nil, knowledgeError(err)
		}
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := validateKnowledgeIndexResponse(first.Scope, first.OperationId, first.SourceDigest, int64(len(documents)), chunkCount, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (client *KnowledgeClient) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if err := ValidateKnowledgeCleanup(request); err != nil {
		return nil, err
	}
	request, ok := proto.Clone(request).(*protocol.KnowledgeCleanupRequest)
	if !ok {
		return nil, status.Error(codes.Internal, "clone knowledge cleanup request")
	}
	ctx, cancel := context.WithTimeout(ctx, knowledgeTimeout)
	defer cancel()
	identity := protocol.NewIdentityClient(client.connection)
	info, err := identity.GetProviderInfo(ctx, &protocol.GetProviderInfoRequest{})
	if err != nil {
		return nil, knowledgeError(err)
	}
	if info.Name != client.name || info.Dimension != "knowledge" {
		return nil, status.Error(codes.FailedPrecondition, "knowledge provider identity mismatch")
	}
	capabilities, err := identity.GetProviderCapabilities(ctx, &protocol.GetProviderCapabilitiesRequest{})
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := validateKnowledgeCleanupCapabilities(capabilities.Knowledge, request); err != nil {
		return nil, err
	}
	response, err := protocol.NewKnowledgeExtensionClient(client.connection).Cleanup(ctx, request)
	if err != nil {
		return nil, knowledgeError(err)
	}
	if err := validateKnowledgeCleanupResponse(request, response); err != nil {
		return nil, err
	}
	return response, nil
}
