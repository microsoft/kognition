// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

var (
	serverURL    string
	outputFormat string
)

var namespace string

// Kube flags
var (
	kubeContext   string
	kubeNamespace string
	kubeService   string
	kubePort      int
)

// mode controls how the CLI communicates with the backend.
// "standalone" — all operations via kognition-apiserver HTTP API.
// "kubernetes" — CRD operations via client-go, runtime ops proxy to agent Services.
// "auto" — detect from kubeconfig availability.
var cliMode string

// k8sMode indicates the CLI is operating in kubernetes mode (set after resolution).
var k8sMode bool

// httpClient is the shared HTTP client used by all commands.
// In kube mode it includes auth transport; otherwise it's http.DefaultClient.
var httpClient = http.DefaultClient

var rootCmd = &cobra.Command{
	Use:   "kognitionctl",
	Short: "CLI for the Kognition cognitive orchestration platform",
	Long:  `kognitionctl is the command-line interface for managing agents, workflows, and resources on the Kognition platform.`,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		return resolveMode(cmd)
	},
}

// resolveMode determines the CLI execution mode.
func resolveMode(cmd *cobra.Command) error {
	switch cliMode {
	case "standalone":
		// Use direct HTTP mode — serverURL is already set or defaulted.
		k8sMode = false
		return configureStandaloneAuth()
	case "kubernetes":
		// Force kubernetes mode via client-go.
		k8sMode = true
		if err := initK8sClient(); err != nil {
			return fmt.Errorf("kubernetes mode: %w", err)
		}
		// Also resolve kube transport for runtime commands (chat/goal) that use HTTP.
		proxy, err := resolveKubeTransport(kubeContext, kubeNamespace, kubeService, kubePort)
		if err == nil {
			serverURL = proxy.baseURL
			httpClient = proxy.client
		}
		return nil
	case "auto":
		// If --server was explicitly passed, use standalone.
		if cmd.Flags().Changed("server") {
			k8sMode = false
			return configureStandaloneAuth()
		}
		// Try kubernetes mode.
		if err := initK8sClient(); err == nil {
			k8sMode = true
			// Also set up HTTP transport for runtime commands.
			proxy, err := resolveKubeTransport(kubeContext, kubeNamespace, kubeService, kubePort)
			if err == nil {
				serverURL = proxy.baseURL
				httpClient = proxy.client
			}
			return nil
		}
		// Fallback: standalone with default server URL.
		k8sMode = false
		return configureStandaloneAuth()
	default:
		return fmt.Errorf("invalid mode %q: must be standalone, kubernetes, or auto", cliMode)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cliMode, "mode", "auto", "CLI mode: standalone, kubernetes, or auto")
	rootCmd.PersistentFlags().StringVar(&serverURL, "server", "http://localhost:8080", "Kognition API server URL")
	rootCmd.PersistentFlags().StringVar(&apiTokenFile, "token-file", "", "File containing the bearer token for an authenticated standalone API server (or set KOGNITION_API_TOKEN)")
	rootCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "", "Output format (yaml|json|wide)")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Namespace")

	rootCmd.PersistentFlags().StringVar(&kubeContext, "kube-context", "", "Kubeconfig context (default: current-context)")
	rootCmd.PersistentFlags().StringVar(&kubeNamespace, "kube-namespace", "kognition", "Namespace where kognition-apiserver runs")
	rootCmd.PersistentFlags().StringVar(&kubeService, "kube-service", "kognition-apiserver", "Kubernetes service name")
	rootCmd.PersistentFlags().IntVar(&kubePort, "kube-port", 8080, "Service port")
}

// SetPluginMode configures the root command for kubectl plugin usage.
func SetPluginMode() {
	rootCmd.Use = "kubectl-kognition"
	rootCmd.Short = "kubectl plugin for the Kognition cognitive orchestration platform"
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
