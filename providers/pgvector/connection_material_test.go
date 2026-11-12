// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/providers/pgvector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func databaseCertificate(t *testing.T, host string, expired bool) ([]byte, tls.Certificate) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test database CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if expired {
		leaf.NotAfter = time.Now().Add(-time.Minute)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func materialSecrets(ca []byte) (*corev1.Secret, *corev1.Secret) {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "database"}, Data: map[string][]byte{"username": []byte("knowledge_reader"), "password": []byte("private-value ' \\ = @?#")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "database-ca"}, Data: map[string][]byte{"ca.crt": ca}}
}

func secretReader(t *testing.T, secrets ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(secrets...).Build()
}

func databaseHandshake(config *tls.Config, certificate tls.Certificate, maxVersion uint16) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	clientConnection, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		return err
	}
	defer clientConnection.Close()
	serverConnection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer serverConnection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		server := tls.Server(serverConnection, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS10, MaxVersion: maxVersion})
		_ = server.HandshakeContext(ctx)
		_ = serverConnection.Close()
	}()
	err = tls.Client(clientConnection, config).HandshakeContext(ctx)
	_ = clientConnection.Close()
	<-serverDone
	return err
}

func TestConnectionMaterialVerifiesDatabaseTLS(t *testing.T) {
	host := "postgres.tenant-a.svc.cluster.local"
	for _, test := range []struct {
		name, host         string
		expired, untrusted bool
		maxVersion         uint16
		wantError          bool
	}{
		{name: "trusted TLS 1.3", host: host, maxVersion: tls.VersionTLS13},
		{name: "trusted TLS 1.2", host: host, maxVersion: tls.VersionTLS12},
		{name: "wrong host", host: "other.example", maxVersion: tls.VersionTLS13, wantError: true},
		{name: "untrusted CA", host: host, untrusted: true, maxVersion: tls.VersionTLS13, wantError: true},
		{name: "expired certificate", host: host, expired: true, maxVersion: tls.VersionTLS13, wantError: true},
		{name: "TLS downgrade", host: host, maxVersion: tls.VersionTLS11, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ca, certificate := databaseCertificate(t, test.host, test.expired)
			if test.untrusted {
				ca, _ = databaseCertificate(t, host, false)
			}
			credentials, roots := materialSecrets(ca)
			material, err := pgvector.LoadConnectionMaterial(context.Background(), secretReader(t, credentials, roots), validBackend())
			if err != nil {
				t.Fatal(err)
			}
			if material.Username != string(credentials.Data["username"]) || material.Password != string(credentials.Data["password"]) {
				t.Fatal("credentials were modified")
			}
			for _, format := range []string{"%v", "%+v", "%#v"} {
				if strings.Contains(fmt.Sprintf(format, material), "private-value") {
					t.Fatal("formatted material exposes credentials")
				}
			}
			err = databaseHandshake(material.TLS, certificate, test.maxVersion)
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("TLS test timed out instead of validating the peer: %v", err)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("handshake error = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestConnectionMaterialRejectsUnavailableOrInvalidSecrets(t *testing.T) {
	ca, _ := databaseCertificate(t, "postgres.tenant-a.svc.cluster.local", false)
	for name, mutate := range map[string]func(*corev1.Secret, *corev1.Secret){
		"credentials in another namespace": func(credentials, roots *corev1.Secret) { credentials.Namespace = "tenant-b" },
		"CA in another namespace":          func(credentials, roots *corev1.Secret) { roots.Namespace = "tenant-b" },
		"missing username":                 func(credentials, roots *corev1.Secret) { delete(credentials.Data, "username") },
		"missing password":                 func(credentials, roots *corev1.Secret) { delete(credentials.Data, "password") },
		"oversized username": func(credentials, roots *corev1.Secret) {
			credentials.Data["username"] = []byte(strings.Repeat("a", 64))
		},
		"oversized password": func(credentials, roots *corev1.Secret) {
			credentials.Data["password"] = []byte(strings.Repeat("a", 65537))
		},
		"NUL password":          func(credentials, roots *corev1.Secret) { credentials.Data["password"] = []byte("private-value\x00") },
		"NUL username":          func(credentials, roots *corev1.Secret) { credentials.Data["username"] = []byte("private-value\x00") },
		"invalid UTF8":          func(credentials, roots *corev1.Secret) { credentials.Data["username"] = []byte{0xff} },
		"invalid password UTF8": func(credentials, roots *corev1.Secret) { credentials.Data["password"] = []byte{0xff} },
		"missing CA":            func(credentials, roots *corev1.Secret) { delete(roots.Data, "ca.crt") },
		"invalid CA":            func(credentials, roots *corev1.Secret) { roots.Data["ca.crt"] = []byte("private-value") },
		"partially valid CA": func(credentials, roots *corev1.Secret) {
			roots.Data["ca.crt"] = append(append([]byte{}, ca...), []byte("private-value")...)
		},
		"invalid certificate DER": func(credentials, roots *corev1.Secret) {
			roots.Data["ca.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("private-value")})
		},
		"oversized CA": func(credentials, roots *corev1.Secret) { roots.Data["ca.crt"] = []byte(strings.Repeat("a", (1<<20)+1)) },
		"deleting credentials": func(credentials, roots *corev1.Secret) {
			stamp := metav1.Now()
			credentials.DeletionTimestamp = &stamp
			credentials.Finalizers = []string{"test"}
		},
		"deleting CA": func(credentials, roots *corev1.Secret) {
			stamp := metav1.Now()
			roots.DeletionTimestamp = &stamp
			roots.Finalizers = []string{"test"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			credentials, roots := materialSecrets(ca)
			mutate(credentials, roots)
			material, err := pgvector.LoadConnectionMaterial(context.Background(), secretReader(t, credentials, roots), validBackend())
			if material != nil || status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("must fail without returning credentials: %v", err)
			}
		})
	}
}

func TestConnectionMaterialRotationAndSharedSecret(t *testing.T) {
	ca, certificate := databaseCertificate(t, "postgres.tenant-a.svc.cluster.local", false)
	credentials, _ := materialSecrets(ca)
	credentials.Data["ca.crt"] = ca
	configuration := validBackend()
	configuration.Object["spec"].(map[string]interface{})["tls"].(map[string]interface{})["caSecretRef"].(map[string]interface{})["name"] = "database"
	reader := secretReader(t, credentials)
	first, err := pgvector.LoadConnectionMaterial(context.Background(), reader, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Get(context.Background(), client.ObjectKeyFromObject(credentials), credentials); err != nil {
		t.Fatal(err)
	}
	credentials.Data["password"] = []byte("rotated-private-value")
	if err := reader.Update(context.Background(), credentials); err != nil {
		t.Fatal(err)
	}
	second, err := pgvector.LoadConnectionMaterial(context.Background(), reader, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if first.Password == second.Password || second.Password != "rotated-private-value" {
		t.Fatal("Secret rotation was not observed")
	}
	second.TLS.ServerName = "other.example"
	if err := databaseHandshake(first.TLS, certificate, tls.VersionTLS13); err != nil {
		t.Fatalf("returned material aliases another load: %v", err)
	}
}

func TestConnectionMaterialRejectsMalformedBlockBeforeValidCA(t *testing.T) {
	ca, _ := databaseCertificate(t, "postgres.tenant-a.svc.cluster.local", false)
	malformed := []byte("-----BEGIN CERTIFICATE-----\nnot base64!\n-----END CERTIFICATE-----\n")
	credentials, roots := materialSecrets(append(malformed, ca...))
	material, err := pgvector.LoadConnectionMaterial(context.Background(), secretReader(t, credentials, roots), validBackend())
	if material != nil || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("malformed CA block must not be skipped: %v", err)
	}
}

func TestConnectionMaterialCABundleFormats(t *testing.T) {
	host := "postgres.tenant-a.svc.cluster.local"
	ca, certificate := databaseCertificate(t, host, false)
	otherCA, _ := databaseCertificate(t, host, false)
	block, _ := pem.Decode(ca)
	block.Headers = map[string]string{"Unexpected": "header"}
	for _, test := range []struct {
		name      string
		bundle    []byte
		wantError bool
	}{
		{name: "multiple roots and CRLF", bundle: []byte(strings.ReplaceAll(string(otherCA)+string(ca), "\n", "\r\n"))},
		{name: "leaf certificate", bundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), wantError: true},
		{name: "PEM headers", bundle: pem.EncodeToMemory(block), wantError: true},
		{name: "leading garbage", bundle: append([]byte("garbage\n"), ca...), wantError: true},
		{name: "nested block", bundle: append([]byte("-----BEGIN CERTIFICATE-----\n"), ca...), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials, roots := materialSecrets(test.bundle)
			material, err := pgvector.LoadConnectionMaterial(context.Background(), secretReader(t, credentials, roots), validBackend())
			if test.wantError {
				if material != nil || status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("invalid CA bundle accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := databaseHandshake(material.TLS, certificate, tls.VersionTLS13); err != nil {
				t.Fatalf("valid bundle rejected: %v", err)
			}
		})
	}
}

func TestConnectionMaterialSystemRootsAndInvalidInput(t *testing.T) {
	credentials, _ := materialSecrets(nil)
	reader := secretReader(t, credentials)
	configuration := validBackend()
	delete(configuration.Object["spec"].(map[string]interface{})["tls"].(map[string]interface{}), "caSecretRef")
	material, err := pgvector.LoadConnectionMaterial(context.Background(), reader, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if material.TLS.RootCAs != nil || material.TLS.InsecureSkipVerify || material.TLS.ServerName != "postgres.tenant-a.svc.cluster.local" || material.TLS.MinVersion < tls.VersionTLS12 {
		t.Fatal("unsafe system trust configuration")
	}
	_, selfSigned := databaseCertificate(t, material.TLS.ServerName, false)
	if err := databaseHandshake(material.TLS, selfSigned, tls.VersionTLS13); err == nil {
		t.Fatal("system trust accepted a private test CA")
	}
	if material, err := pgvector.LoadConnectionMaterial(context.Background(), nil, configuration); material != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil reader: %v", err)
	}
	if material, err := pgvector.LoadConnectionMaterial(context.Background(), reader, nil); material != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil configuration: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if material, err := pgvector.LoadConnectionMaterial(ctx, reader, configuration); material != nil || status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if material, err := pgvector.LoadConnectionMaterial(ctx, reader, configuration); material != nil || status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline: %v", err)
	}
}

type failingSecretReader struct{ client.Reader }

func (failingSecretReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("upstream private-value")
}

func TestConnectionMaterialSanitizesReaderErrors(t *testing.T) {
	material, err := pgvector.LoadConnectionMaterial(context.Background(), failingSecretReader{}, validBackend())
	if material != nil || status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("unsanitized upstream error: %v", err)
	}
}
