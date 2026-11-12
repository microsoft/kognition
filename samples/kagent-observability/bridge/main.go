// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	api "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/compat/kagent"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	filename, namespace := os.Getenv("KOGNITION_BRIDGE_KUBECONFIG"), os.Getenv("KOGNITION_BRIDGE_NAMESPACE")
	if filename == "" || namespace == "" {
		return errors.New("explicit bridge kubeconfig and namespace are required")
	}
	config, err := clientcmd.BuildConfigFromFlags("", filename)
	if err != nil {
		return errors.New("operator bridge Kubernetes configuration unavailable")
	}
	if config.TLSClientConfig.Insecure {
		return errors.New("insecure bridge Kubernetes transport is forbidden")
	}
	if err := api.AddToScheme(scheme.Scheme); err != nil {
		return err
	}
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return err
	}
	jobs := kagent.GovernedPromQLJobs{Client: admin, Namespace: namespace, AgentName: os.Getenv("KOGNITION_BRIDGE_PROMQL_AGENT")}
	bridge, err := kagent.NewObservabilityBridge(kagent.ObservabilityConfig{
		Namespace: namespace, Kubeconfig: filename, PrometheusURL: os.Getenv("KOGNITION_BRIDGE_PROMETHEUS_URL"), GrafanaURL: os.Getenv("KOGNITION_BRIDGE_GRAFANA_URL"), GrafanaToken: os.Getenv("KOGNITION_BRIDGE_GRAFANA_TOKEN"), DashboardPrefix: os.Getenv("KOGNITION_BRIDGE_DASHBOARD_PREFIX"), FolderUID: os.Getenv("KOGNITION_BRIDGE_FOLDER_UID"), GeneratePromQL: jobs.Generate,
		RunKubectl: func(ctx context.Context, args []string) (string, error) {
			output, err := exec.CommandContext(ctx, "kubectl", args...).Output()
			if err != nil {
				return "", errors.New("reviewed Kubernetes read failed")
			}
			if len(output) > 1<<20 {
				return "", errors.New("Kubernetes read exceeds response bound")
			}
			return string(output), nil
		},
	})
	if err != nil {
		return err
	}
	address := os.Getenv("KOGNITION_BRIDGE_LISTEN")
	if address == "" {
		address = ":8080"
	}
	server := &http.Server{Addr: address, Handler: bridge, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
