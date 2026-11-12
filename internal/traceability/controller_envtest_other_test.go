// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration && !windows

package traceability

import "sigs.k8s.io/controller-runtime/pkg/envtest"

func stopControllerEnvtest(environment *envtest.Environment) error {
	return environment.Stop()
}
