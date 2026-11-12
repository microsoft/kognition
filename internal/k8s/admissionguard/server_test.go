// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admissionguard

import "testing"

func TestGuardConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*ServerOptions){
		"valid":                 func(*ServerOptions) {},
		"empty-installation":    func(options *ServerOptions) { options.InstallationNamespace = "" },
		"default-identity":      func(options *ServerOptions) { options.ControllerServiceAccount = "default" },
		"empty-identity":        func(options *ServerOptions) { options.ControllerServiceAccount = "" },
		"installation-workload": func(options *ServerOptions) { options.WorkloadNamespaces = []string{"control"} },
		"wildcard":              func(options *ServerOptions) { options.WorkloadNamespaces = []string{"*"} },
		"empty-workload":        func(options *ServerOptions) { options.WorkloadNamespaces = []string{""} },
		"no-tls":                func(options *ServerOptions) { options.CertDir = "" },
		"bad-port":              func(options *ServerOptions) { options.Port = 0 },
		"missing-baseline":      func(options *ServerOptions) { options.BaselinePolicyName = "" },
		"invalid-baseline":      func(options *ServerOptions) { options.BaselinePolicyName = "*" },
		"missing-release":       func(options *ServerOptions) { options.ReleaseName = "" },
		"invalid-release":       func(options *ServerOptions) { options.ReleaseName = "*" },
	} {
		t.Run(name, func(t *testing.T) {
			options := ServerOptions{ReleaseName: "test", InstallationNamespace: "control", ControllerServiceAccount: "controller", BaselinePolicyName: "stdio-baseline", WorkloadNamespaces: []string{"tenant"}, CertDir: "/tls", Port: 9443}
			mutate(&options)
			handler, err := newHandler(options)
			if (err == nil) != (name == "valid") {
				t.Fatalf("handler=%+v error=%v", handler, err)
			}
			if err == nil && (handler.ReleaseName != options.ReleaseName || handler.InstallationNamespace != options.InstallationNamespace || handler.BaselinePolicyName != options.BaselinePolicyName) {
				t.Fatal("baseline ownership configuration was lost")
			}
		})
	}
}
