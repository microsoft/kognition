// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

type failingCompletionStore struct{ *memStore }

func (*failingCompletionStore) CompleteModelDispatch(context.Context, string, string, string, json.RawMessage) error {
	return errors.New("completion storage unavailable")
}

func TestCPIDispatcher_RecoveryFailsClosed(t *testing.T) {
	for _, scenario := range []string{"pending claim", "completion write failure", "corrupt completion"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			persistent := newMemStore()
			seedCPIResources(t, persistent)
			instance := &v1alpha1.AgentInstance{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "active", Namespace: "default", UID: "active-uid"},
				Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}},
			}
			driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "private output", Usage: cpi.Usage{Reported: true}}}
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			var backend store.Store = persistent
			if scenario == "completion write failure" {
				backend = &failingCompletionStore{persistent}
			} else {
				if claimed, err := persistent.ClaimModelDispatch(ctx, "default", agentInstanceReservationID(instance), "claimed-session"); err != nil || !claimed {
					t.Fatalf("claim: claimed=%t error=%v", claimed, err)
				}
				if scenario == "corrupt completion" {
					if err := persistent.CompleteModelDispatch(ctx, "default", agentInstanceReservationID(instance), "claimed-session", json.RawMessage(`null`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				dispatcher := NewCPIDispatcher(backend, registry, zaptest.NewLogger(t))
				if session, err := dispatcher.Dispatch(ctx, instance); !errors.Is(err, ErrDispatchAccountingUncertain) || session != "" {
					t.Fatalf("uncertain dispatch: returnedSession=%t error=%v", session != "", err)
				}
			}
			expectedCalls := 0
			if scenario == "completion write failure" {
				expectedCalls = 1
			}
			if driver.calls != expectedCalls {
				t.Fatalf("provider calls=%d, want %d", driver.calls, expectedCalls)
			}
			record, err := persistent.GetModelDispatch(ctx, "default", agentInstanceReservationID(instance))
			if err != nil {
				t.Fatal(err)
			}
			instance.Status.SessionID = record.SessionID
			restarted := NewCPIDispatcher(backend, registry, zaptest.NewLogger(t))
			if status, err := restarted.Status(ctx, instance); !errors.Is(err, ErrDispatchAccountingUncertain) || status.Completed || status.Output != "" {
				t.Fatalf("uncertain status: completed=%t error=%v", status.Completed, err)
			}
			if err := restarted.Terminate(ctx, instance, "finished"); !errors.Is(err, ErrDispatchAccountingUncertain) {
				t.Fatalf("uncertain termination: %v", err)
			}
		})
	}
}

func TestCPIDispatcher_RecoveryBoundToInstanceAndSession(t *testing.T) {
	ctx := context.Background()
	persistent := newMemStore()
	seedCPIResources(t, persistent)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "active", Namespace: "default", UID: "active-uid"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}},
	}
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "private output", Usage: cpi.Usage{Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(persistent, registry, zaptest.NewLogger(t))
	session, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = session
	for _, scenario := range []string{"namespace", "instance UID", "session"} {
		t.Run(scenario, func(t *testing.T) {
			other := *instance
			switch scenario {
			case "namespace":
				other.ObjectMeta.Namespace = "other"
			case "instance UID":
				other.ObjectMeta.UID = "replacement-uid"
			case "session":
				other.Status.SessionID = "other-session"
			}
			for _, current := range []*cpiDispatcher{dispatcher, NewCPIDispatcher(persistent, registry, zaptest.NewLogger(t))} {
				if status, err := current.Status(ctx, &other); err == nil || status.Output != "" || status.Completed {
					t.Fatalf("cross-identity lookup: completed=%t error=%v", status.Completed, err)
				}
			}
			if scenario == "session" {
				if _, err := dispatcher.Dispatch(ctx, &other); !errors.Is(err, ErrDispatchAccountingUncertain) {
					t.Fatalf("mismatched session replay: %v", err)
				}
			}
		})
	}
	if driver.calls != 1 {
		t.Fatalf("provider calls=%d", driver.calls)
	}
}
