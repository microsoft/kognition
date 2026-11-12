// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestManagerSynchronizesKnowledgeBaseUIDIndex(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}

	crdPath, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "kognition", "crds"))
	if err != nil {
		t.Fatalf("resolve CRD path: %v", err)
	}
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		_ = testEnvironment.Stop()
	})

	kubeconfigPath := filepath.Join(t.TempDir(), "config")
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   restConfig.Host,
		CertificateAuthorityData: restConfig.CAData,
	}
	kubeconfig.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: restConfig.CertData,
		ClientKeyData:         restConfig.KeyData,
		Token:                 restConfig.BearerToken,
	}
	kubeconfig.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	kubeconfig.CurrentContext = "envtest"
	if err := clientcmd.WriteToFile(*kubeconfig, kubeconfigPath); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	manager, err := newManager(runtimeConfig{watchedNamespace: "tenant-a"})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := index.Register(ctx, manager); err != nil {
		t.Fatalf("register index: %v", err)
	}

	managerErrors := make(chan error, 1)
	go func() { managerErrors <- manager.Start(ctx) }()
	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("manager cache did not synchronize")
	}
	index.MarkSynced()

	if err := manager.GetClient().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	knowledgeBase := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "manuals"},
		Spec: k8s.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "pgvector",
		},
	}
	if err := manager.GetClient().Create(ctx, knowledgeBase); err != nil {
		t.Fatalf("create KnowledgeBase: %v", err)
	}

	waitForIndexEntry(t, func() bool {
		resolved, err := index.Lookup("tenant-a", string(knowledgeBase.UID))
		return err == nil && resolved == (types.NamespacedName{Namespace: "tenant-a", Name: "manuals"})
	})

	if err := manager.GetClient().Delete(ctx, knowledgeBase); err != nil {
		t.Fatalf("delete KnowledgeBase: %v", err)
	}
	waitForIndexEntry(t, func() bool {
		_, err := index.Lookup("tenant-a", string(knowledgeBase.UID))
		return err != nil
	})
	exerciseRunUntilListenerFailure(t)

	cancel()
	select {
	case err := <-managerErrors:
		if err != nil {
			t.Fatalf("manager stopped with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not stop")
	}
}

func exerciseRunUntilListenerFailure(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener address: %v", err)
	}
	defer listener.Close()

	clearKnowledgeEnv(t)
	certPath, keyPath, caPath := writeCertificateMaterial(t, t.TempDir())
	t.Setenv("KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", listener.Addr().String())
	t.Setenv("KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", "127.0.0.1:0")
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://example.test/knowledge")
	t.Setenv("KOGNITION_KNOWLEDGE_WATCH_NAMESPACE", "tenant-a")
	// The run stops at the occupied listener before any database connection is attempted.
	t.Setenv("KOGNITION_PGVECTOR_DATABASE_ADDRESS", "127.0.0.1:5432")

	err = run(zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "open knowledge gRPC listener") {
		t.Fatalf("run error = %v, want occupied listener error", err)
	}
}

func waitForIndexEntry(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for KnowledgeBase index update")
}
