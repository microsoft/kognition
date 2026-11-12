// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrNestedOutcomeUncertain = errors.New("governed nested model outcome is uncertain; retain AgentJob and do not retry")

type GovernedPromQLJobs struct {
	Client    client.Client
	Namespace string
	AgentName string
}

func (jobs GovernedPromQLJobs) Generate(ctx context.Context, invocationID, description string) (string, error) {
	if ctx.Err() != nil {
		return "", ErrNestedOutcomeUncertain
	}
	if jobs.Client == nil || jobs.Namespace == "" || jobs.AgentName == "" || strings.TrimSpace(invocationID) == "" || len(invocationID) > 128 || strings.TrimSpace(description) == "" || len(description) > 65536 {
		return "", errors.New("governed PromQL job requires explicit reviewed identity and bounded input")
	}
	hash := sha256.Sum256([]byte(jobs.Namespace + "\x00" + jobs.AgentName + "\x00" + invocationID))
	name := "kagent-promql-" + hex.EncodeToString(hash[:])[:40]
	zero := int32(0)
	expected := &api.AgentJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: jobs.Namespace, Annotations: map[string]string{"kognition.dev/tool-invocation-id": invocationID}}, Spec: api.AgentJobSpec{AgentRef: &api.LocalObjectReference{Name: jobs.AgentName}, Input: description, BackoffLimit: &zero}}
	if err := jobs.Client.Create(ctx, expected); err != nil && !apierrors.IsAlreadyExists(err) {
		if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) && !apierrors.IsNotFound(err) {
			return "", ErrNestedOutcomeUncertain
		}
		return "", fmt.Errorf("submit governed PromQL job: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		live := &api.AgentJob{}
		if err := jobs.Client.Get(ctx, client.ObjectKey{Namespace: jobs.Namespace, Name: name}, live); err != nil {
			return "", ErrNestedOutcomeUncertain
		}
		if live.Spec.AgentRef == nil || live.Spec.AgentRef.Name != jobs.AgentName || live.Spec.AgentSpec != nil || live.Spec.AgentRevision != nil || live.Spec.MemoryClaimRef != nil || live.Spec.Input != description || live.Spec.BackoffLimit == nil || *live.Spec.BackoffLimit != 0 || live.Annotations["kognition.dev/tool-invocation-id"] != invocationID || live.DeletionTimestamp != nil {
			return "", errors.New("governed PromQL invocation identity or input changed")
		}
		switch live.Status.Phase {
		case "Ready":
			if live.Status.Output == "" {
				return "", errors.New("governed PromQL job completed without output")
			}
			return live.Status.Output, nil
		case "Failed", "Terminated":
			return "", errors.New("governed PromQL job failed; automatic retry is disabled")
		case "", "Pending", "Running":
		default:
			return "", ErrNestedOutcomeUncertain
		}
		select {
		case <-ctx.Done():
			return "", ErrNestedOutcomeUncertain
		case <-ticker.C:
		}
	}
}
