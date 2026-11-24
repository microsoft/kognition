// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package k8sledger

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
)

// The real API server must arbitrate concurrent writers of one ledger.
func TestLedgerCompareAndSwapAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	env := &envtest.Environment{}
	rc, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Create(bg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "wfc"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ConfigMaps: cs.CoreV1().ConfigMaps("wfc"), Name: "wfc-run1"}
	led, _ := NewLedger(cfg)
	v1, err := led.Create(bg, valid())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := valid()
			st.Tasks = []collaboration.Task{{ID: "t" + strconv.Itoa(i), State: collaboration.TaskPending}}
			_, results[i] = led.Save(bg, st, v1)
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range results {
		switch {
		case e == nil:
			wins++
		case !errors.Is(e, driver.ErrConflict):
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("%d writers won the same version", wins)
	}
	h, _ := NewHandles(cfg)
	if err := h.Put(bg, "t1.1", "remote"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := h.Get(bg, "t1.1"); err != nil || !ok || v != "remote" {
		t.Fatalf("handle: %q %v %v", v, ok, err)
	}
}
