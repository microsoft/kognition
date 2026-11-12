// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type runtimeApprovalWriteReadFailureClient struct {
	client.Client
	target   client.Object
	writeErr error
	readErr  error
	wrote    bool
}

func (fault *runtimeApprovalWriteReadFailureClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if fault.wrote && reflect.TypeOf(object) == reflect.TypeOf(fault.target) {
		return fault.readErr
	}
	return fault.Client.Get(ctx, key, object, options...)
}

func (fault *runtimeApprovalWriteReadFailureClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := fault.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if reflect.TypeOf(object) == reflect.TypeOf(fault.target) {
		fault.wrote = true
		return fault.writeErr
	}
	return nil
}

func (fault *runtimeApprovalWriteReadFailureClient) Status() client.SubResourceWriter {
	return &runtimeApprovalWriteReadFailureWriter{SubResourceWriter: fault.Client.Status(), fault: fault}
}

type runtimeApprovalWriteReadFailureWriter struct {
	client.SubResourceWriter
	fault *runtimeApprovalWriteReadFailureClient
}

func (writer *runtimeApprovalWriteReadFailureWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if err := writer.SubResourceWriter.Update(ctx, object, options...); err != nil {
		return err
	}
	writer.fault.wrote = true
	return writer.fault.writeErr
}

func (writer *runtimeApprovalWriteReadFailureWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	if err := writer.SubResourceWriter.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	writer.fault.wrote = true
	return writer.fault.writeErr
}

func TestKubernetesRuntimeApprovalCreatePreservesWriteAndReadFailures(t *testing.T) {
	for _, target := range []client.Object{&k8s.RuntimeApproval{}, &k8s.RuntimeApprovalQuotaTicket{}} {
		t.Run(reflect.TypeOf(target).Elem().Name(), func(t *testing.T) {
			base := newFakeClient(t)
			fault := &runtimeApprovalWriteReadFailureClient{Client: base, target: target, writeErr: errors.New("write response lost"), readErr: errors.New("reread unavailable")}
			adapter := newRuntimeApprovalTestStore(t, fault)
			adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
			proposal := kubernetesRuntimeApprovalFixture(t, "create-read-failure", api.RuntimeApprovalDecisionPending)
			created, _, err := adapter.CreateOrGetRuntimeApproval(context.Background(), proposal, kubernetesRuntimeApprovalPayload, time.Minute)
			if created || !errors.Is(err, fault.writeErr) || !errors.Is(err, fault.readErr) {
				t.Fatalf("ambiguous create = %v, %v; want both write and reread errors", created, err)
			}
			if err := base.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", proposal.Spec.InvocationID)}, target); err != nil {
				t.Fatalf("expected persisted write behind ambiguous response: %v", err)
			}
			adapter.client = base
			_, recovered, err := adapter.CreateOrGetRuntimeApproval(context.Background(), proposal, kubernetesRuntimeApprovalPayload, time.Minute)
			if err != nil || recovered.Status.DecisionState != api.RuntimeApprovalDecisionPending {
				t.Fatalf("recovery = state %q, error %v", recovered.Status.DecisionState, err)
			}
		})
	}
}

func TestKubernetesRuntimeApprovalTransitionPreservesWriteAndReadFailures(t *testing.T) {
	for _, operation := range []string{"decision", "execution"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
			adapter := newExecutionTestAdapter(t, &now)
			var approval api.RuntimeApproval
			if operation == "execution" {
				approval = newExecutionReadyKubernetesApproval(t, ctx, adapter, operation, api.RuntimeApprovalDecisionNotRequired)
			} else {
				_, created, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, operation, api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				approval = created
			}
			base := adapter.client
			fault := &runtimeApprovalWriteReadFailureClient{Client: base, target: &k8s.RuntimeApproval{}, writeErr: errors.New("status response lost"), readErr: errors.New("reread unavailable")}
			adapter.client = fault
			var err error
			if operation == "execution" {
				_, err = adapter.PrepareRuntimeApproval(ctx, "team-a", operation, approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute))
			} else {
				_, err = adapter.DecideRuntimeApproval(ctx, "team-a", operation, store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "issuer", Subject: "operator"})
			}
			if !errors.Is(err, fault.writeErr) || !errors.Is(err, fault.readErr) {
				t.Fatalf("ambiguous transition error = %v; want both write and reread errors", err)
			}
			adapter.client = base
			recovered, err := adapter.GetRuntimeApprovalByInvocation(ctx, "team-a", operation)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "decision" && recovered.Status.DecisionState != api.RuntimeApprovalDecisionApproved {
				t.Fatalf("persisted decision = %q", recovered.Status.DecisionState)
			}
			if operation == "execution" && recovered.Status.PreparationToken == "" {
				t.Fatal("persisted preparation was lost")
			}
		})
	}
}
