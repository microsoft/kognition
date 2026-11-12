// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
)

func TestPostRendererSubprocess(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	executable := filepath.Join(test.TempDir(), "kognition-install.exe")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", executable, ".").CombinedOutput(); err != nil {
		test.Fatalf("build installation CLI: %v: %s", err, output)
	}
	manifest, err := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--kube-version", "1.30.3", "--no-hooks", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12", "--set", "k8sController.managedNamespaces[0]=tenant").Output()
	if err != nil {
		test.Fatalf("render reviewed manifest: %v", err)
	}
	documents, err := installation.Documents(manifest)
	if err != nil {
		test.Fatal(err)
	}
	var reordered bytes.Buffer
	for index := len(documents) - 1; index >= 0; index-- {
		reordered.WriteString("---\n")
		if err := json.NewEncoder(&reordered).Encode(documents[index].Object); err != nil {
			test.Fatal(err)
		}
	}
	for _, scenario := range []string{"exact", "reordered", "render-drift", "plan-drift", "artifact-drift", "oversized"} {
		test.Run(scenario, func(test *testing.T) {
			directory := test.TempDir()
			artifact := installation.Artifact{Path: filepath.Join(directory, "manifest.yaml"), SHA256: fmt.Sprintf("%x", sha256.Sum256(manifest))}
			if err := os.WriteFile(artifact.Path, manifest, 0600); err != nil {
				test.Fatal(err)
			}
			plan := installation.Plan{Version: 1, Kubeconfig: "target.yaml", Endpoint: "https://127.0.0.1:12345", ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid", Release: "security-test", Chart: installation.Artifact{Path: "reviewed.tgz", SHA256: strings.Repeat("a", 64)}, Values: artifact, Manifest: artifact, Controllers: []installation.Controller{}, Grants: []installation.ObjectRef{}, LegacyPolicies: []installation.ObjectRef{}, WorkloadNamespaceUIDs: map[string]string{"tenant": "tenant-uid"}}
			content, err := json.Marshal(plan)
			if err != nil {
				test.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(content))
			path := filepath.Join(directory, "reviewed plan.json")
			input := manifest
			switch scenario {
			case "reordered":
				input = reordered.Bytes()
			case "render-drift":
				input = append(append([]byte{}, manifest...), []byte("\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: unreviewed, namespace: control}\n")...)
			case "plan-drift":
				content = append(content, '\n')
			case "artifact-drift":
				if err := os.WriteFile(artifact.Path, []byte("changed"), 0600); err != nil {
					test.Fatal(err)
				}
			case "oversized":
				input = bytes.Repeat([]byte(" "), 32*1024*1024+1)
			}
			if err := os.WriteFile(path, content, 0600); err != nil {
				test.Fatal(err)
			}
			command := exec.CommandContext(ctx, executable, "post-render")
			command.Env = helmEnvironment(os.Environ(), path, digest)
			command.Stdin = bytes.NewReader(input)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err = command.Run()
			if scenario == "exact" || scenario == "reordered" {
				if err != nil || !bytes.Equal(stdout.Bytes(), input) {
					test.Fatalf("reviewed render rejected or altered: %v: %s", err, &stderr)
				}
			} else if err == nil || stdout.Len() != 0 || stderr.Len() == 0 {
				test.Fatalf("unreviewed render not rejected cleanly: %v, stdout bytes=%d, stderr=%s", err, stdout.Len(), &stderr)
			}
		})
	}
}
