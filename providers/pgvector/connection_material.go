// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ConnectionMaterial struct {
	Username string
	Password string
	TLS      *tls.Config
}

func (ConnectionMaterial) String() string   { return "pgvector connection material (redacted)" }
func (ConnectionMaterial) GoString() string { return "pgvector.ConnectionMaterial{redacted}" }

func LoadConnectionMaterial(ctx context.Context, reader client.Reader, configuration *unstructured.Unstructured) (*ConnectionMaterial, error) {
	spec, err := DecodeBackend(configuration)
	if err != nil || reader == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector connection configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	loadSecret := func(name string) (*corev1.Secret, error) {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: configuration.GetNamespace(), Name: name}
		if err := reader.Get(ctx, key, secret); err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.FailedPrecondition, "pgvector Secret is unavailable")
		}
		if secret.Namespace != key.Namespace || secret.Name != key.Name || !secret.DeletionTimestamp.IsZero() {
			return nil, status.Error(codes.FailedPrecondition, "pgvector Secret is unavailable")
		}
		return secret, nil
	}
	credentials, err := loadSecret(spec.CredentialsSecretRef.Name)
	if err != nil {
		return nil, err
	}
	username := string(credentials.Data[spec.CredentialsSecretRef.UsernameKey])
	password := string(credentials.Data[spec.CredentialsSecretRef.PasswordKey])
	if username == "" || len(username) > 63 || password == "" || len(password) > 65536 ||
		!utf8.ValidString(username) || !utf8.ValidString(password) || strings.ContainsRune(username, 0) || strings.ContainsRune(password, 0) {
		return nil, status.Error(codes.FailedPrecondition, "invalid pgvector credentials")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: spec.Host}
	if reference := spec.TLS.CASecretRef; reference != nil {
		caSecret := credentials
		if reference.Name != credentials.Name {
			caSecret, err = loadSecret(reference.Name)
			if err != nil {
				return nil, err
			}
		}
		encoded := bytes.TrimSpace(caSecret.Data[reference.Key])
		if len(encoded) == 0 || len(encoded) > 1<<20 {
			return nil, status.Error(codes.FailedPrecondition, "invalid pgvector CA bundle")
		}
		roots := x509.NewCertPool()
		for len(encoded) != 0 {
			beginMarker := []byte("-----BEGIN CERTIFICATE-----")
			endMarker := []byte("-----END CERTIFICATE-----")
			end := bytes.Index(encoded, endMarker)
			if !bytes.HasPrefix(encoded, beginMarker) || end < 0 {
				return nil, status.Error(codes.FailedPrecondition, "invalid pgvector CA bundle")
			}
			end += len(endMarker)
			block, remaining := pem.Decode(encoded[:end])
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(remaining) != 0 || bytes.Count(encoded[:end], beginMarker) != 1 {
				return nil, status.Error(codes.FailedPrecondition, "invalid pgvector CA bundle")
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
				return nil, status.Error(codes.FailedPrecondition, "invalid pgvector CA bundle")
			}
			roots.AddCert(certificate)
			encoded = bytes.TrimSpace(encoded[end:])
		}
		tlsConfig.RootCAs = roots
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return &ConnectionMaterial{Username: username, Password: password, TLS: tlsConfig}, nil
}
