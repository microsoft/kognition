// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestSnapshotCredentialSources(test *testing.T) {
	for _, mode := range []string{"files", "exec", "auth-provider"} {
		test.Run(mode, func(test *testing.T) {
			directory := test.TempDir()
			artifact := Artifact{Path: filepath.Join(directory, "source"), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("reviewed")))}
			if err := os.WriteFile(artifact.Path, []byte("reviewed"), 0600); err != nil {
				test.Fatal(err)
			}
			config := clientcmdapi.NewConfig()
			config.CurrentContext = "target"
			config.Contexts["target"] = &clientcmdapi.Context{Cluster: "target", AuthInfo: "admin"}
			config.Clusters["target"] = &clientcmdapi.Cluster{Server: "https://reviewed.example", CertificateAuthority: artifact.Path}
			credentials := &clientcmdapi.AuthInfo{TokenFile: artifact.Path, ClientCertificate: artifact.Path, ClientKey: artifact.Path}
			if mode == "exec" {
				credentials.Exec = &clientcmdapi.ExecConfig{Command: "never-execute"}
			}
			if mode == "auth-provider" {
				credentials.AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "never-execute"}
			}
			config.AuthInfos["admin"] = credentials
			path := filepath.Join(directory, "config")
			if err := clientcmd.WriteToFile(*config, path); err != nil {
				test.Fatal(err)
			}
			captured, _, cleanup, err := Snapshot(Plan{Kubeconfig: path, Chart: artifact, Values: artifact, Manifest: artifact})
			if mode != "files" {
				if err == nil {
					cleanup()
					test.Fatal("dynamic credentials accepted")
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(cleanup)
			if err := os.Remove(artifact.Path); err != nil {
				test.Fatal(err)
			}
			snapshot, err := clientcmd.LoadFromFile(captured.Kubeconfig)
			if err != nil {
				test.Fatal(err)
			}
			auth := snapshot.AuthInfos["admin"]
			cluster := snapshot.Clusters["target"]
			if auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || cluster.CertificateAuthority != "" || auth.Token != "reviewed" || string(auth.ClientCertificateData) != "reviewed" || string(auth.ClientKeyData) != "reviewed" || string(cluster.CertificateAuthorityData) != "reviewed" {
				test.Fatal("snapshot retains a mutable credential source")
			}
		})
	}
}

func TestSnapshotCapturesArtifactsAndCredentials(test *testing.T) {
	directory := test.TempDir()
	artifact := Artifact{Path: filepath.Join(directory, "source"), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("reviewed")))}
	if err := os.WriteFile(artifact.Path, []byte("reviewed"), 0600); err != nil {
		test.Fatal(err)
	}
	kubeconfig := filepath.Join(directory, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\ncurrent-context: target\ncontexts:\n- name: target\n  context:\n    cluster: target\nclusters:\n- name: target\n  cluster:\n    server: https://reviewed.example\n"), 0600); err != nil {
		test.Fatal(err)
	}
	plan := Plan{Kubeconfig: kubeconfig, Chart: artifact, Values: artifact, Manifest: artifact, PreviousManifest: &artifact, PreviousDeployedManifest: &artifact}
	captured, path, cleanup, err := Snapshot(plan)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(cleanup)
	if plan.PreviousManifest.Path != artifact.Path || plan.PreviousDeployedManifest.Path != artifact.Path {
		test.Fatal("source plan mutated")
	}
	if err := os.WriteFile(artifact.Path, []byte("replaced"), 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.Remove(kubeconfig); err != nil {
		test.Fatal(err)
	}
	for _, snapshot := range []Artifact{captured.Chart, captured.Values, captured.Manifest, *captured.PreviousManifest, *captured.PreviousDeployedManifest} {
		content, err := ReadArtifact(snapshot)
		if err != nil || string(content) != "reviewed" {
			test.Fatalf("snapshot changed: %s, %v", content, err)
		}
	}
	config, err := clientcmd.BuildConfigFromFlags("", captured.Kubeconfig)
	if err != nil || config.Host != "https://reviewed.example" {
		test.Fatalf("captured target differs: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		test.Fatalf("snapshot not removed: %v", err)
	}
}
