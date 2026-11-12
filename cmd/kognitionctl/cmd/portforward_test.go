// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func resetPortForwardState(t *testing.T) {
	t.Helper()
	pfCacheMu.Lock()
	previousCache := pfCache
	previousInflight := pfInflight
	pfCache = map[string]*agentPortForward{}
	pfInflight = map[string]chan struct{}{}
	pfCacheMu.Unlock()
	t.Cleanup(func() {
		pfCacheMu.Lock()
		pfCache = previousCache
		pfInflight = previousInflight
		pfCacheMu.Unlock()
	})
}

func TestGetOrCreatePortForward_ReusesCachedTunnel(t *testing.T) {
	resetPortForwardState(t)
	want := &agentPortForward{LocalPort: 10001, stopChan: make(chan struct{})}
	pfCache["tenant/agent"] = want

	got, err := getOrCreatePortForward(context.Background(), "tenant", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("port-forward = %p, want cached %p", got, want)
	}
}

func TestGetOrCreatePortForward_InFlightWaitHonorsCancellation(t *testing.T) {
	resetPortForwardState(t)
	pfInflight["tenant/agent"] = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := getOrCreatePortForward(ctx, "tenant", "agent")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestGetOrCreatePortForward_InFlightCompletionRequiresCachedTunnel(t *testing.T) {
	resetPortForwardState(t)
	done := make(chan struct{})
	pfInflight["tenant/agent"] = done
	close(done)

	_, err := getOrCreatePortForward(context.Background(), "tenant", "agent")
	if err == nil || !strings.Contains(err.Error(), "was not cached") {
		t.Fatalf("error = %v, want missing cache error", err)
	}
}

func TestStartPortForward_RequiresKubernetesClient(t *testing.T) {
	previousClient := k8sCoreClient
	k8sCoreClient = nil
	t.Cleanup(func() { k8sCoreClient = previousClient })

	_, err := startPortForward(context.Background(), "tenant", "agent")
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("error = %v, want uninitialized client error", err)
	}
}

func TestStartPortForward_DiscoversAgentPodBySourceIdentity(t *testing.T) {
	previousClient := k8sCoreClient
	client := fake.NewSimpleClientset()
	k8sCoreClient = client
	t.Cleanup(func() { k8sCoreClient = previousClient })
	t.Setenv("KOGNITION_AGENT_NAMESPACE", "shared-runtime")

	var namespace, selector string
	client.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		listAction := action.(ktesting.ListAction)
		namespace = action.GetNamespace()
		selector = listAction.GetListRestrictions().Labels.String()
		return false, nil, nil
	})

	_, err := startPortForward(context.Background(), "tenant-a", "planner")
	if err == nil || !strings.Contains(err.Error(), `no pods found for agent "planner"`) {
		t.Fatalf("error = %v, want no-pods discovery error", err)
	}
	if namespace != "shared-runtime" {
		t.Fatalf("pod namespace = %q, want shared-runtime", namespace)
	}
	for _, label := range []string{
		"kognition.dev/agent-name=planner",
		"kognition.dev/agent-namespace=tenant-a",
		"app.kubernetes.io/managed-by=kognition",
	} {
		if !strings.Contains(selector, label) {
			t.Fatalf("selector = %q, missing %q", selector, label)
		}
	}
}

func TestStartPortForward_ReportsPodListFailure(t *testing.T) {
	previousClient := k8sCoreClient
	client := fake.NewSimpleClientset()
	k8sCoreClient = client
	t.Cleanup(func() { k8sCoreClient = previousClient })
	t.Setenv("KOGNITION_AGENT_NAMESPACE", "")

	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pod API unavailable")
	})

	_, err := startPortForward(context.Background(), "tenant-a", "planner")
	if err == nil || !strings.Contains(err.Error(), "list pods for agent planner: pod API unavailable") {
		t.Fatalf("error = %v, want wrapped pod list failure", err)
	}
}

func TestGetOrCreatePortForward_FailedCreationClearsInFlightState(t *testing.T) {
	resetPortForwardState(t)
	previousClient := k8sCoreClient
	k8sCoreClient = nil
	t.Cleanup(func() { k8sCoreClient = previousClient })

	_, err := getOrCreatePortForward(context.Background(), "tenant", "agent")
	if err == nil {
		t.Fatal("expected port-forward creation to fail")
	}
	pfCacheMu.Lock()
	_, cached := pfCache["tenant/agent"]
	_, inflight := pfInflight["tenant/agent"]
	pfCacheMu.Unlock()
	if cached || inflight {
		t.Fatalf("failed creation left state: cached=%t inflight=%t", cached, inflight)
	}
}

func TestInvalidatePortForward_ClosesAndEvictsOnlyTarget(t *testing.T) {
	targetKey := "test-namespace/target-agent"
	otherKey := "test-namespace/other-agent"
	target := &agentPortForward{LocalPort: 10001, stopChan: make(chan struct{})}
	other := &agentPortForward{LocalPort: 10002, stopChan: make(chan struct{})}

	pfCacheMu.Lock()
	pfCache[targetKey] = target
	pfCache[otherKey] = other
	pfCacheMu.Unlock()
	t.Cleanup(func() {
		pfCacheMu.Lock()
		delete(pfCache, targetKey)
		delete(pfCache, otherKey)
		pfCacheMu.Unlock()
		select {
		case <-other.stopChan:
		default:
			other.Close()
		}
	})

	invalidatePortForward("test-namespace", "target-agent")
	invalidatePortForward("test-namespace", "target-agent")

	select {
	case <-target.stopChan:
	default:
		t.Fatal("target tunnel was not closed")
	}
	pfCacheMu.Lock()
	_, targetCached := pfCache[targetKey]
	cachedOther := pfCache[otherKey]
	pfCacheMu.Unlock()
	if targetCached {
		t.Fatal("target tunnel remains cached")
	}
	if cachedOther != other {
		t.Fatalf("unrelated cached tunnel = %p, want %p", cachedOther, other)
	}
	select {
	case <-other.stopChan:
		t.Fatal("unrelated tunnel was closed")
	default:
	}
}
