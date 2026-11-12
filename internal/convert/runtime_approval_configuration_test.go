// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"reflect"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestRuntimeApprovalConfigurationDigestRoundTrip(t *testing.T) {
	approval := &api.RuntimeApproval{
		Spec: api.RuntimeApprovalSpec{
			AgentInstanceRef: api.RuntimeApprovalReference{
				Kind: "AgentInstance", Scope: "Namespaced", Namespace: "team-a", Name: "instance",
				UID: "instance-uid", ResourceVersion: 17, ConfigurationDigest: strings.Repeat("a", 64),
			},
		},
	}
	converted := RuntimeApprovalToK8s(approval)
	if converted.Spec.AgentInstanceRef.ConfigurationDigest != approval.Spec.AgentInstanceRef.ConfigurationDigest {
		t.Fatal("Kubernetes conversion lost the configuration digest")
	}
	restored := RuntimeApprovalFromK8s(converted)
	if !reflect.DeepEqual(restored.Spec.AgentInstanceRef, approval.Spec.AgentInstanceRef) {
		t.Fatal("RuntimeApproval reference did not round-trip")
	}
}
