// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kognition-ai/kognition/internal/k8s/admissionguard"
	ctrl "sigs.k8s.io/controller-runtime"
)

func runAdmissionGuardCommand(args []string, stderr io.Writer) int {
	var options admissionguard.ServerOptions
	var namespaces string
	flags := flag.NewFlagSet("admission-guard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.ReleaseName, "release-name", "", "Owning Helm release")
	flags.StringVar(&options.InstallationNamespace, "installation-namespace", "", "Installation namespace")
	flags.StringVar(&options.ControllerServiceAccount, "controller-service-account", "", "Guarded controller ServiceAccount")
	flags.StringVar(&options.BaselinePolicyName, "baseline-policy-name", "", "Independent stdio isolation NetworkPolicy")
	flags.StringVar(&namespaces, "workload-namespaces", "", "Comma-separated dedicated workload namespaces")
	flags.StringVar(&options.CertDir, "cert-dir", "/tls", "Serving TLS directory")
	flags.IntVar(&options.Port, "port", 9443, "HTTPS admission port")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "unexpected admission guard arguments")
		return 2
	}
	if namespaces != "" {
		options.WorkloadNamespaces = strings.Split(namespaces, ",")
	}
	config, err := ctrl.GetConfig()
	if err == nil {
		err = admissionguard.Serve(ctrl.SetupSignalHandler(), config, options)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
