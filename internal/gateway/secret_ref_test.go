// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"errors"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestResolveAPIKeyFromSecretRef(t *testing.T) {
	lookup := func(name string) (string, bool) {
		return map[string]string{"GATEWAY_KEY": "from-env"}[name], name == "GATEWAY_KEY"
	}
	for _, test := range []struct {
		name   string
		secret v1alpha1.SecretRef
		want   string
	}{
		{name: "env", secret: v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "GATEWAY_KEY", Key: "token"}}, want: "from-env"},
		{name: "internal", secret: v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{internalSecretDataAnnotation: `{"type":"api-key","token":"from-internal"}`}}, Spec: v1alpha1.SecretRefSpec{BackendType: "internal", Key: "token"}}, want: "from-internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := ResolveAPIKeyFromSecretRef(&test.secret, lookup)
			if err != nil || actual != test.want {
				t.Fatalf("ResolveAPIKeyFromSecretRef() = %q, %v; want %q, nil", actual, err, test.want)
			}
		})
	}
	_, err := ResolveAPIKeyFromSecretRef(&v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "internal", Key: "token"}}, lookup)
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("missing internal credential error = %v, want ErrCredentialUnavailable", err)
	}
}
