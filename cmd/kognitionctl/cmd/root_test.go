// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveMode_RejectsUnknownMode(t *testing.T) {
	previousMode := cliMode
	cliMode = "kubernets"
	t.Cleanup(func() { cliMode = previousMode })

	err := resolveMode(&cobra.Command{})

	if err == nil || !strings.Contains(err.Error(), `invalid mode "kubernets"`) {
		t.Fatalf("resolveMode() error = %v, want invalid mode error", err)
	}
}

func TestResolveMode_SelectsStandaloneWithoutKubernetesInitialization(t *testing.T) {
	tests := []struct {
		name           string
		mode           string
		explicitServer bool
	}{
		{name: "forced standalone", mode: "standalone"},
		{name: "auto with explicit server", mode: "auto", explicitServer: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousMode, previousK8sMode := cliMode, k8sMode
			cliMode, k8sMode = tt.mode, true
			t.Cleanup(func() {
				cliMode, k8sMode = previousMode, previousK8sMode
			})

			cmd := &cobra.Command{}
			cmd.Flags().String("server", "http://localhost:8080", "")
			if tt.explicitServer {
				if err := cmd.Flags().Set("server", "https://api.example.test"); err != nil {
					t.Fatal(err)
				}
			}

			if err := resolveMode(cmd); err != nil {
				t.Fatalf("resolveMode() error = %v", err)
			}
			if k8sMode {
				t.Fatal("resolveMode() selected kubernetes mode")
			}
		})
	}
}

func TestResolveMode_AutoFallsBackWhenKubeconfigIsUnavailable(t *testing.T) {
	previousMode, previousK8sMode := cliMode, k8sMode
	cliMode, k8sMode = "auto", true
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-config"))
	t.Cleanup(func() {
		cliMode, k8sMode = previousMode, previousK8sMode
	})

	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatalf("resolveMode() error = %v", err)
	}
	if k8sMode {
		t.Fatal("resolveMode() did not fall back to standalone mode")
	}
}

func TestResolveMode_KubernetesConfiguresAPIProxy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	config := strings.Join([]string{
		"apiVersion: v1",
		"kind: Config",
		"clusters:",
		"- name: test-cluster",
		"  cluster:",
		"    server: https://kube.example.test",
		"contexts:",
		"- name: test-context",
		"  context:",
		"    cluster: test-cluster",
		"    user: test-user",
		"current-context: test-context",
		"users:",
		"- name: test-user",
		"  user:",
		"    token: test-token",
	}, "\n")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", configPath)

	previousMode, previousK8sMode := cliMode, k8sMode
	previousURL, previousClient := serverURL, httpClient
	previousDynamic, previousCore, previousConfig := k8sDynClient, k8sCoreClient, k8sRESTConfig
	cliMode, k8sMode = "kubernetes", false
	t.Cleanup(func() {
		cliMode, k8sMode = previousMode, previousK8sMode
		serverURL, httpClient = previousURL, previousClient
		k8sDynClient, k8sCoreClient, k8sRESTConfig = previousDynamic, previousCore, previousConfig
	})

	if err := resolveMode(&cobra.Command{}); err != nil {
		t.Fatalf("resolveMode() error = %v", err)
	}
	if !k8sMode {
		t.Fatal("resolveMode() did not select kubernetes mode")
	}
	wantURL := "https://kube.example.test/api/v1/namespaces/kognition/services/kognition-apiserver:8080/proxy"
	if serverURL != wantURL {
		t.Fatalf("serverURL = %q, want %q", serverURL, wantURL)
	}
	if httpClient == nil || httpClient == previousClient {
		t.Fatal("resolveMode() did not configure the Kubernetes HTTP client")
	}
}

func TestSetPluginMode(t *testing.T) {
	// Save original values
	origUse := rootCmd.Use
	origShort := rootCmd.Short
	defer func() {
		rootCmd.Use = origUse
		rootCmd.Short = origShort
	}()

	SetPluginMode()

	if rootCmd.Use != "kubectl-kognition" {
		t.Errorf("SetPluginMode() Use = %q, want %q", rootCmd.Use, "kubectl-kognition")
	}
	if rootCmd.Short == "" {
		t.Error("SetPluginMode() Short should not be empty")
	}
	if rootCmd.Short != "kubectl plugin for the Kognition cognitive orchestration platform" {
		t.Errorf("SetPluginMode() Short = %q, want %q", rootCmd.Short, "kubectl plugin for the Kognition cognitive orchestration platform")
	}
}

func TestRootCmd_HasExpectedFlags(t *testing.T) {
	flags := []string{"server", "output", "namespace", "kube-context", "kube-namespace", "kube-service", "kube-port"}
	for _, name := range flags {
		t.Run(name, func(t *testing.T) {
			f := rootCmd.PersistentFlags().Lookup(name)
			if f == nil {
				t.Errorf("expected persistent flag %q to be registered", name)
			}
		})
	}
}

func TestRootCmd_DefaultValues(t *testing.T) {
	tests := []struct {
		flag string
		want string
	}{
		{"server", "http://localhost:8080"},
		{"namespace", "default"},
		{"kube-namespace", "kognition"},
		{"kube-service", "kognition-apiserver"},
		{"kube-port", "8080"},
	}
	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			f := rootCmd.PersistentFlags().Lookup(tt.flag)
			if f == nil {
				t.Fatalf("flag %q not found", tt.flag)
			}
			if f.DefValue != tt.want {
				t.Errorf("flag %q default = %q, want %q", tt.flag, f.DefValue, tt.want)
			}
		})
	}
}

func TestRootCmd_HasSubcommands(t *testing.T) {
	cmds := rootCmd.Commands()
	if len(cmds) == 0 {
		t.Fatal("rootCmd has no subcommands")
	}

	// Check that status is registered
	found := false
	for _, c := range cmds {
		if c.Use == "status [agent-name]" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'status' subcommand to be registered")
	}
}
