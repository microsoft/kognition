// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGatewayReceiptsIsolateWritersAndReplayAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	cluster := newFakeClient(t)
	for _, writer := range []string{"tool-gateway", "agent-communication-gateway"} {
		first := NewStoreAdapterWithControlNamespace(cluster, "control")
		if err := first.ConfigureToolInvocationReceiptWriter(writer); err != nil {
			t.Fatal(err)
		}
		receipt := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "same-invocation", Binding: writer, Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()}
		if created, _, err := first.ClaimToolInvocation(ctx, receipt); err != nil || !created {
			t.Fatalf("%s claim: created=%t err=%v", writer, created, err)
		}
		now := time.Now().UTC()
		receipt.Status, receipt.CompletedAt, receipt.Output = store.ToolInvocationSucceeded, &now, json.RawMessage(`{"writer":"`+writer+`"}`)
		if err := first.CompleteToolInvocation(ctx, receipt); err != nil {
			t.Fatal(err)
		}
		replica := NewStoreAdapterWithControlNamespace(cluster, "control")
		if err := replica.ConfigureToolInvocationReceiptWriter(writer); err != nil {
			t.Fatal(err)
		}
		if created, replay, err := replica.ClaimToolInvocation(ctx, receipt); err != nil || created || !reflect.DeepEqual(replay, receipt) {
			t.Fatalf("%s terminal replay changed: created=%t receipt=%+v err=%v", writer, created, replay, err)
		}
	}
	var objects corev1.ConfigMapList
	if err := cluster.List(ctx, &objects, client.InNamespace("control")); err != nil || len(objects.Items) != 2 {
		t.Fatalf("isolated gateway ledgers: count=%d err=%v", len(objects.Items), err)
	}
}

func TestGatewayReceiptsKeepLegacyOutcomesReadOnly(t *testing.T) {
	for _, status := range []store.ToolInvocationReceiptStatus{store.ToolInvocationPending, store.ToolInvocationAwaitingApproval, store.ToolInvocationPrepared, store.ToolInvocationSucceeded, store.ToolInvocationFailed} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			cluster := newFakeClient(t)
			legacy := NewStoreAdapterWithControlNamespace(cluster, "control")
			receipt := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "retained", Binding: "legacy-binding", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()}
			if _, _, err := legacy.ClaimToolInvocation(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			receipt.Status = status
			if status == store.ToolInvocationPrepared {
				deadline := time.Now().UTC().Add(time.Minute)
				receipt.PreparationToken, receipt.PreparationDeadline = "retained-preparation", &deadline
				if _, _, err := legacy.PrepareToolInvocation(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			} else if status != store.ToolInvocationPending {
				now := time.Now().UTC()
				receipt.CompletedAt, receipt.Output = &now, json.RawMessage(`{"retained":true}`)
				if err := legacy.CompleteToolInvocation(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			}
			before := &corev1.ConfigMap{}
			if err := cluster.Get(ctx, legacy.toolInvocationReceiptObjectKey("tenant"), before); err != nil {
				t.Fatal(err)
			}
			terminal := status == store.ToolInvocationSucceeded || status == store.ToolInvocationFailed
			for _, writer := range []string{"tool-gateway", "agent-communication-gateway"} {
				gateway := NewStoreAdapterWithControlNamespace(cluster, "control")
				if err := gateway.ConfigureToolInvocationReceiptWriter(writer); err != nil {
					t.Fatal(err)
				}
				claimed, replay, err := gateway.ClaimToolInvocation(ctx, receipt)
				if claimed || terminal && (err != nil || !reflect.DeepEqual(replay, receipt)) || !terminal && !errors.Is(err, store.ErrConflict) {
					t.Fatalf("legacy %s claim crossed its fence: claimed=%t err=%v", status, claimed, err)
				}
				changed := receipt
				changed.Binding = "different-action"
				if _, _, err := gateway.ClaimToolInvocation(ctx, changed); !errors.Is(err, store.ErrConflict) {
					t.Fatal("legacy binding change did not fail closed", err)
				}
				prepared := receipt
				prepared.Status, prepared.PreparationToken = store.ToolInvocationPrepared, "new-preparation"
				deadline := time.Now().UTC().Add(time.Minute)
				prepared.PreparationDeadline = &deadline
				if _, _, err := gateway.PrepareToolInvocation(ctx, prepared); terminal && err != nil || !terminal && !errors.Is(err, store.ErrConflict) {
					t.Fatal("legacy preparation crossed its fence", err)
				}
				if released, err := gateway.ReleasePreparedToolInvocation(ctx, "tenant", "retained", "retained-preparation"); released || terminal && err != nil || !terminal && !errors.Is(err, store.ErrConflict) {
					t.Fatal("legacy reservation was released", released, err)
				}
				changed.Status = store.ToolInvocationSucceeded
				if err := gateway.CompleteToolInvocation(ctx, changed); !errors.Is(err, store.ErrConflict) {
					t.Fatal("legacy completion was rewritten", err)
				}
				fresh := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "fresh", Binding: writer, Status: store.ToolInvocationPending}
				if created, _, err := gateway.ClaimToolInvocation(ctx, fresh); !created || err != nil {
					t.Fatal("unrelated new gateway work blocked", err)
				}
			}
			after := &corev1.ConfigMap{}
			if err := cluster.Get(ctx, client.ObjectKeyFromObject(before), after); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("legacy ledger bytes or identity changed", err)
			}
		})
	}
}

func TestGatewayReceiptWriterConfigurationFailsClosed(t *testing.T) {
	gateway := NewStoreAdapterWithControlNamespace(newFakeClient(t), "control")
	for _, writer := range []string{"", "controller", "memory-policy-gateway", "model-gateway"} {
		if err := gateway.ConfigureToolInvocationReceiptWriter(writer); err == nil {
			t.Fatal("unsupported receipt writer accepted", writer)
		}
	}
	if err := NewStoreAdapter(newFakeClient(t)).ConfigureToolInvocationReceiptWriter("tool-gateway"); err == nil {
		t.Fatal("unscoped gateway accepted")
	}
	if err := gateway.ConfigureToolInvocationReceiptWriter("tool-gateway"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.ConfigureToolInvocationReceiptWriter("agent-communication-gateway"); err == nil {
		t.Fatal("configured writer was reassigned")
	}
}

type unavailableLegacyReceiptReader struct{ client.Reader }

func (unavailableLegacyReceiptReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("authoritative receipt read unavailable")
}

func TestGatewayReceiptsRejectUnavailableOrMalformedLegacyEvidence(t *testing.T) {
	ctx := context.Background()
	for _, failure := range []string{"unavailable", "missing-payload", "invalid-json", "unowned"} {
		t.Run(failure, func(t *testing.T) {
			cluster := newFakeClient(t)
			legacy := NewStoreAdapterWithControlNamespace(cluster, "control")
			receipt := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "uncertain", Binding: "binding", Status: store.ToolInvocationPending}
			if _, _, err := legacy.ClaimToolInvocation(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			object := &corev1.ConfigMap{}
			if err := cluster.Get(ctx, legacy.toolInvocationReceiptObjectKey("tenant"), object); err != nil {
				t.Fatal(err)
			}
			gateway := NewStoreAdapterWithControlNamespace(cluster, "control")
			if err := gateway.ConfigureToolInvocationReceiptWriter("tool-gateway"); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "unavailable":
				gateway.ConfigureRuntimeApprovalReader(unavailableLegacyReceiptReader{Reader: cluster})
			case "missing-payload":
				object.Data = nil
			case "invalid-json":
				object.Data["receipts.json"] = "{"
			case "unowned":
				object.Labels = nil
			}
			if err := cluster.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if created, _, err := gateway.ClaimToolInvocation(ctx, receipt); created || err == nil {
				t.Fatal("missing legacy evidence authorized new execution", err)
			}
			var items corev1.ConfigMapList
			if err := cluster.List(ctx, &items); err != nil || len(items.Items) != 1 {
				t.Fatal("legacy lookup failure created a replacement ledger", err)
			}
		})
	}
}
