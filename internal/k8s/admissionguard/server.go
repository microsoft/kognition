// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admissionguard

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type ServerOptions struct {
	ReleaseName              string
	InstallationNamespace    string
	ControllerServiceAccount string
	BaselinePolicyName       string
	WorkloadNamespaces       []string
	CertDir                  string
	Port                     int
}

func Serve(ctx context.Context, config *rest.Config, options ServerOptions) error {
	handler, err := newHandler(options)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create admission guard reader: %w", err)
	}
	handler.Reader = reader
	server := webhook.NewServer(webhook.Options{Port: options.Port, CertDir: options.CertDir})
	server.Register("/guard", &admission.Webhook{Handler: handler})
	server.Register("/readyz", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := server.StartedChecker()(request); err != nil {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	return server.Start(ctx)
}

func newHandler(options ServerOptions) (*AttachHandler, error) {
	if len(validation.IsDNS1123Label(options.InstallationNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(options.ReleaseName)) != 0 ||
		len(validation.IsDNS1123Subdomain(options.BaselinePolicyName)) != 0 ||
		len(validation.IsDNS1123Subdomain(options.ControllerServiceAccount)) != 0 || options.ControllerServiceAccount == "default" ||
		strings.TrimSpace(options.CertDir) == "" || options.Port < 1 || options.Port > 65535 {
		return nil, fmt.Errorf("admission guard requires an installation namespace, dedicated controller identity, TLS directory, and valid port")
	}
	namespaces := make(map[string]bool, len(options.WorkloadNamespaces))
	for _, namespace := range options.WorkloadNamespaces {
		if len(validation.IsDNS1123Label(namespace)) != 0 || namespace == options.InstallationNamespace {
			return nil, fmt.Errorf("invalid admission guard workload namespace %q", namespace)
		}
		namespaces[namespace] = true
	}
	return &AttachHandler{ControllerUsername: "system:serviceaccount:" + options.InstallationNamespace + ":" + options.ControllerServiceAccount, WorkloadNamespaces: namespaces, BaselinePolicyName: options.BaselinePolicyName, ReleaseName: options.ReleaseName, InstallationNamespace: options.InstallationNamespace}, nil
}
