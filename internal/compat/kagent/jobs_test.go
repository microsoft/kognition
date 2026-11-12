// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kagent

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGovernedPromQLJobsFenceInputAndReuseCompletedOutcome(test *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	admin := fake.NewClientBuilder().WithScheme(scheme).Build()
	jobs := GovernedPromQLJobs{Client: admin, Namespace: "fixture", AgentName: "promql-agent"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := jobs.Generate(ctx, "invocation-1", "generate uptime query"); !errors.Is(err, ErrNestedOutcomeUncertain) {
		test.Fatalf("unfinished outcome did not fence: %v", err)
	}
	var list api.AgentJobList
	if err := admin.List(context.Background(), &list); err != nil || len(list.Items) != 1 {
		test.Fatal("one durable nested job required")
	}
	job := list.Items[0]
	if job.Spec.AgentSpec != nil || job.Spec.AgentRef.Name != "promql-agent" || *job.Spec.BackoffLimit != 0 {
		test.Fatal("nested model escaped reviewed child Agent or enabled retries")
	}
	job.Status.Phase, job.Status.Output = "Ready", "up"
	if err := admin.Update(context.Background(), &job); err != nil {
		test.Fatal(err)
	}
	output, err := jobs.Generate(context.Background(), "invocation-1", "generate uptime query")
	if err != nil || output != "up" {
		test.Fatalf("durable completed outcome not reused: %q %v", output, err)
	}
	if _, err := jobs.Generate(context.Background(), "invocation-1", "different input"); err == nil {
		test.Fatal("same invocation rebound to different input")
	}
	if err := admin.List(context.Background(), &list, client.InNamespace("fixture")); err != nil || len(list.Items) != 1 {
		test.Fatal("duplicate nested paid operation created")
	}
}
