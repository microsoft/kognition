// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
)

func TestActivationRejectsNilInventoryAfterCiliumAPIPolicy(test *testing.T) {
	manifest, err := activation.Parse(bytes.NewReader(encodeCiliumAPITarget(test, ciliumAPITarget(test))))
	if err != nil {
		test.Fatal(err)
	}
	manifest.Objects = append(manifest.Objects, nil)
	if err := activation.Verify(context.Background(), nil, manifest); err == nil || !strings.Contains(err.Error(), "nil bootstrap object") {
		test.Fatalf("malformed inventory must fail closed before accessing the cluster: %v", err)
	}
}
