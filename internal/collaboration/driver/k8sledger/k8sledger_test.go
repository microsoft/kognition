// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package k8sledger

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
)

var bg = context.Background()

func valid() collaboration.State {
	return collaboration.State{Version: collaboration.StateVersion, Phase: collaboration.PhaseRunning}
}

// newClient emulates API-server resourceVersion semantics that the fake lacks.
func newClient(t *testing.T) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(metav1.Object).SetResourceVersion("1")
		return false, nil, nil
	})
	cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		upd := a.(k8stesting.UpdateAction)
		obj := upd.GetObject().(metav1.Object)
		cur, err := cs.Tracker().Get(upd.GetResource(), upd.GetNamespace(), obj.GetName())
		if err != nil {
			return true, nil, err
		}
		rv := cur.(metav1.Object).GetResourceVersion()
		if obj.GetResourceVersion() != rv {
			return true, nil, apierrors.NewConflict(upd.GetResource().GroupResource(), obj.GetName(), errors.New("stale"))
		}
		n, _ := strconv.Atoi(rv)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		return false, nil, nil
	})
	return cs
}

func config(cs *fake.Clientset) Config {
	return Config{ConfigMaps: cs.CoreV1().ConfigMaps("ns"), Name: "wfc-run1",
		Labels: map[string]string{"app": "kognition"},
		Owners: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "u1"}}}
}

func TestLedgerContract(t *testing.T) {
	cs := newClient(t)
	led, err := NewLedger(config(cs))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := led.Load(bg); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("load before create: %v", err)
	}
	if _, err := led.Save(bg, valid(), "1"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("save before create: %v", err)
	}
	v1, err := led.Create(bg, valid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.Create(bg, valid()); !errors.Is(err, driver.ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	if _, err := led.Save(bg, valid(), ""); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("unversioned save: %v", err)
	}
	v2, err := led.Save(bg, valid(), v1)
	if err != nil || v2 == v1 {
		t.Fatalf("save: %v %q %q", err, v1, v2)
	}
	if _, err := led.Save(bg, valid(), v1); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if _, v, err := led.Load(bg); err != nil || v != v2 {
		t.Fatalf("load: %v %q", err, v)
	}
	cm, _ := cs.CoreV1().ConfigMaps("ns").Get(bg, "wfc-run1", metav1.GetOptions{})
	if cm.Labels["app"] != "kognition" || len(cm.OwnerReferences) != 1 {
		t.Fatalf("metadata not applied: %+v", cm.ObjectMeta)
	}
}

func TestLedgerSurvivesRestartAndRejectsCorruption(t *testing.T) {
	cs := newClient(t)
	first, _ := NewLedger(config(cs))
	if _, err := first.Create(bg, valid()); err != nil {
		t.Fatal(err)
	}
	second, _ := NewLedger(config(cs))
	if _, _, err := second.Load(bg); err != nil {
		t.Fatalf("restart load: %v", err)
	}
	cm, _ := cs.CoreV1().ConfigMaps("ns").Get(bg, "wfc-run1", metav1.GetOptions{})
	cm.Data[StateKey] = "{not json"
	if _, err := cs.CoreV1().ConfigMaps("ns").Update(bg, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.Load(bg); err == nil {
		t.Fatal("corrupt state must not load")
	}
	cm, _ = cs.CoreV1().ConfigMaps("ns").Get(bg, "wfc-run1", metav1.GetOptions{})
	delete(cm.Data, StateKey)
	if _, err := cs.CoreV1().ConfigMaps("ns").Update(bg, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.Load(bg); err == nil {
		t.Fatal("a ledger without state must not load")
	}
}

func TestLedgerSurfacesAPIFailures(t *testing.T) {
	boom := errors.New("api down")
	for _, verb := range []string{"get", "create", "update"} {
		cs := newClient(t)
		led, _ := NewLedger(config(cs))
		v, _ := led.Create(bg, valid())
		cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
		var err error
		switch verb {
		case "get":
			_, _, err = led.Load(bg)
		case "create":
			_, err = led.Create(bg, valid())
		default:
			_, err = led.Save(bg, valid(), v)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("%s: %v", verb, err)
		}
	}
}

func TestLedgerSaveOfDeletedObject(t *testing.T) {
	cs := newClient(t)
	led, _ := NewLedger(config(cs))
	v, _ := led.Create(bg, valid())
	if err := cs.CoreV1().ConfigMaps("ns").Delete(bg, "wfc-run1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Save(bg, valid(), v); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cs := newClient(t)
	for _, c := range []Config{
		{Name: "ok"},
		{ConfigMaps: cs.CoreV1().ConfigMaps("ns"), Name: "Bad_Name"},
		{ConfigMaps: cs.CoreV1().ConfigMaps("ns"), Name: strings.Repeat("a", 250)},
	} {
		if _, err := NewLedger(c); err == nil {
			t.Fatalf("ledger accepted %+v", c.Name)
		}
		if _, err := NewHandles(c); err == nil {
			t.Fatalf("handles accepted %+v", c.Name)
		}
	}
}

func TestHandlesRoundTripAndSurviveRestart(t *testing.T) {
	cs := newClient(t)
	h, _ := NewHandles(config(cs))
	if _, ok, err := h.Get(bg, "t1.1"); err != nil || ok {
		t.Fatalf("empty get: %v %v", ok, err)
	}
	if err := h.Put(bg, "t1.1", "remote-1"); err != nil {
		t.Fatal(err)
	}
	if err := h.Put(bg, "t1.2", "remote-2"); err != nil {
		t.Fatal(err)
	}
	if err := h.Put(bg, "t1.1", "remote-1"); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	if err := h.Put(bg, "t1.1", "other"); err == nil {
		t.Fatal("a handle must not be rebound")
	}
	restarted, _ := NewHandles(config(cs))
	if v, ok, err := restarted.Get(bg, "t1.2"); err != nil || !ok || v != "remote-2" {
		t.Fatalf("restart get: %q %v %v", v, ok, err)
	}
	if _, ok, _ := restarted.Get(bg, "absent"); ok {
		t.Fatal("unknown dispatch found")
	}
}

func TestHandlesRejectInvalidInput(t *testing.T) {
	h, _ := NewHandles(config(newClient(t)))
	for _, c := range [][2]string{{"", "r"}, {"bad key", "r"}, {"../x", "r"}, {"t1", ""}, {"t1", strings.Repeat("r", maxHandleLen+1)}} {
		if err := h.Put(bg, c[0], c[1]); err == nil {
			t.Fatalf("accepted %q=%q", c[0], c[1])
		}
	}
}

func TestHandlesRetryLostRacesThenGiveUp(t *testing.T) {
	cs := newClient(t)
	h, _ := NewHandles(config(cs))
	if err := h.Put(bg, "t1.1", "r1"); err != nil {
		t.Fatal(err)
	}
	conflicts := 2
	cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(a.GetResource().GroupResource(), "x", errors.New("race"))
		}
		return false, nil, nil
	})
	if err := h.Put(bg, "t1.2", "r2"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	conflicts = maxHandleAttempts
	if err := h.Put(bg, "t1.3", "r3"); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("exhausted retries: %v", err)
	}
}

func TestHandlesCreateRaceAndAPIFailures(t *testing.T) {
	boom := errors.New("api down")
	cs := newClient(t)
	h, _ := NewHandles(config(cs))
	raced := false
	cs.PrependReactor("create", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !raced {
			raced = true
			seed := a.(k8stesting.CreateAction).GetObject().DeepCopyObject()
			_ = cs.Tracker().Add(seed)
			return true, nil, apierrors.NewAlreadyExists(a.GetResource().GroupResource(), "x")
		}
		return false, nil, nil
	})
	if err := h.Put(bg, "t1.1", "r1"); err != nil {
		t.Fatalf("create race: %v", err)
	}
	for _, verb := range []string{"get", "update", "create"} {
		cs := newClient(t)
		h, _ := NewHandles(config(cs))
		if verb == "update" {
			_ = h.Put(bg, "t1.1", "r1")
		}
		cs.PrependReactor(verb, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
		if err := h.Put(bg, "t9.9", "r"); !errors.Is(err, boom) {
			t.Fatalf("%s: %v", verb, err)
		}
		if verb == "get" {
			if _, _, err := h.Get(bg, "t9.9"); !errors.Is(err, boom) {
				t.Fatalf("get: %v", err)
			}
		}
	}
}
