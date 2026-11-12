// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// agentPortForward manages a port-forward tunnel to an agent pod.
type agentPortForward struct {
	LocalPort uint16
	stopChan  chan struct{}
}

// Close tears down the port-forward tunnel.
func (pf *agentPortForward) Close() {
	close(pf.stopChan)
}

// portForwardCache caches active port-forwards keyed by "namespace/agentName".
// pfInflight tracks keys that are currently being created to avoid duplicate tunnels.
var (
	pfCache    = map[string]*agentPortForward{}
	pfInflight = map[string]chan struct{}{}
	pfCacheMu  sync.Mutex
)

// getOrCreatePortForward returns a cached or new port-forward to the named agent's pod.
// It uses a per-key "in-flight" channel to avoid creating duplicate tunnels when
// concurrent requests arrive for the same agent (the first creator wins; latecomers
// wait and reuse its result).
func getOrCreatePortForward(ctx context.Context, namespace, agentName string) (*agentPortForward, error) {
	key := namespace + "/" + agentName

	pfCacheMu.Lock()
	// Fast path: already cached.
	if pf, ok := pfCache[key]; ok {
		pfCacheMu.Unlock()
		return pf, nil
	}
	// Check if another goroutine is already creating a port-forward for this key.
	if ch, ok := pfInflight[key]; ok {
		pfCacheMu.Unlock()
		// Wait for the creator to finish, but honour context cancellation.
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		pfCacheMu.Lock()
		pf, ok := pfCache[key]
		pfCacheMu.Unlock()
		if ok {
			return pf, nil
		}
		return nil, fmt.Errorf("port-forward for %s was not cached after in-flight wait", key)
	}
	// We are the creator — register an in-flight signal.
	ch := make(chan struct{})
	pfInflight[key] = ch
	pfCacheMu.Unlock()

	pf, err := startPortForward(ctx, namespace, agentName)

	pfCacheMu.Lock()
	delete(pfInflight, key)
	if err == nil {
		pfCache[key] = pf
	}
	pfCacheMu.Unlock()
	close(ch) // unblock waiters

	return pf, err
}

// invalidatePortForward removes a cached port-forward (e.g. after a connection failure).
func invalidatePortForward(namespace, agentName string) {
	key := namespace + "/" + agentName
	pfCacheMu.Lock()
	if pf, ok := pfCache[key]; ok {
		pf.Close()
		delete(pfCache, key)
	}
	pfCacheMu.Unlock()
}

// startPortForward finds a ready pod for the agent service and sets up a port-forward.
func startPortForward(ctx context.Context, namespace, agentName string) (*agentPortForward, error) {
	if k8sCoreClient == nil {
		return nil, fmt.Errorf("kubernetes client not initialized")
	}

	// Find a pod by its source Agent identity. Runtime pods may be placed in a
	// shared namespace, where the internal workload key is intentionally opaque.
	runtimeNamespace := namespace
	if configured := os.Getenv("KOGNITION_AGENT_NAMESPACE"); configured != "" {
		runtimeNamespace = configured
	}
	labelSelector := fmt.Sprintf("kognition.dev/agent-name=%s,kognition.dev/agent-namespace=%s,app.kubernetes.io/managed-by=kognition", agentName, namespace)
	pods, err := k8sCoreClient.CoreV1().Pods(runtimeNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		Limit:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods for agent %s: %w", agentName, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pods found for agent %q from namespace %q in runtime namespace %q", agentName, namespace, runtimeNamespace)
	}
	podName := pods.Items[0].Name

	// Build the SPDY URL for pod port-forward.
	reqURL := k8sCoreClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(runtimeNamespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(k8sRESTConfig)
	if err != nil {
		return nil, fmt.Errorf("create SPDY transport: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})

	// Port 0 means "pick any free local port".
	fw, err := portforward.New(dialer, []string{"0:8080"}, stopChan, readyChan, os.Stdout, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("create port-forward: %w", err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- fw.ForwardPorts()
	}()

	// Wait for the port-forward to be ready or fail.
	select {
	case <-readyChan:
	case err := <-errChan:
		return nil, fmt.Errorf("port-forward failed: %w", err)
	case <-ctx.Done():
		close(stopChan)
		return nil, ctx.Err()
	}

	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopChan)
		return nil, fmt.Errorf("get forwarded ports: %w", err)
	}

	return &agentPortForward{
		LocalPort: ports[0].Local,
		stopChan:  stopChan,
	}, nil
}
