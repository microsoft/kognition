// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventEmitter emits Kubernetes Events on observed status transitions so the
// dashboard (and `kubectl describe`) can surface controller-side reconciliation
// outcomes through the standard core/v1 Events API.
//
// The emitter is intentionally non-invasive: it inspects status.phase and
// status.conditions after each Reconcile and only writes an event when the
// observed fingerprint changes, leaning on the underlying EventBroadcaster
// to coalesce identical repeats.
type eventEmitter struct {
	client   client.Client
	recorder events.EventRecorder
	kind     string

	mu       sync.Mutex
	lastSeen map[types.NamespacedName]string
}

// setup wires the emitter against the controller-runtime manager. Safe to call
// from each adapter's SetupWithManager.
func (e *eventEmitter) setup(mgr ctrl.Manager, kind, controllerName string) {
	e.client = mgr.GetClient()
	e.recorder = mgr.GetEventRecorder(controllerName)
	e.kind = kind
	e.lastSeen = make(map[types.NamespacedName]string)
}

// emit records an event reflecting the current reconcile outcome for req.
// Safe to call when setup has not run (no-op in that case).
func (e *eventEmitter) emit(ctx context.Context, req ctrl.Request, reconcileErr error) {
	if e == nil || e.recorder == nil || e.client == nil {
		return
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kognition.dev",
		Version: "v1alpha1",
		Kind:    e.kind,
	})
	if err := e.client.Get(ctx, req.NamespacedName, u); err != nil {
		// Object is gone or unreadable — drop dedupe entry so a future
		// reincarnation re-emits, and skip.
		e.mu.Lock()
		delete(e.lastSeen, req.NamespacedName)
		e.mu.Unlock()
		return
	}

	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	fp := fingerprint(reconcileErr, phase, conds)

	e.mu.Lock()
	prev := e.lastSeen[req.NamespacedName]
	if prev == fp {
		e.mu.Unlock()
		return
	}
	e.lastSeen[req.NamespacedName] = fp
	e.mu.Unlock()

	if reconcileErr != nil {
		e.recorder.Eventf(u, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", "%v", reconcileErr)
		return
	}

	reason, message, evType := summarize(phase, conds)
	if reason == "" {
		return
	}
	e.recorder.Eventf(u, nil, evType, reason, "Reconcile", "%s", message)
}

func fingerprint(reconcileErr error, phase string, conds []interface{}) string {
	var b strings.Builder
	if reconcileErr != nil {
		b.WriteString("err=")
		b.WriteString(reconcileErr.Error())
		return b.String()
	}
	b.WriteString("phase=")
	b.WriteString(phase)
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		fmt.Fprintf(&b, "|%s=%s:%s", t, s, r)
	}
	return b.String()
}

// summarize chooses a reason/message/event-type from current phase and
// conditions. Returns reason="" to suppress event emission (e.g. transient
// Pending states).
func summarize(phase string, conds []interface{}) (reason, message, evType string) {
	for _, conditionType := range []string{"Demoted", "Probation"} {
		for _, condition := range conds {
			fields, ok := condition.(map[string]interface{})
			if !ok || fields["type"] != conditionType || fields["status"] != "True" {
				continue
			}
			_, message = pickCondition(conds, conditionType, "True", conditionType, "trust profile state changed")
			return conditionType, message, corev1.EventTypeWarning
		}
	}
	switch strings.ToLower(phase) {
	case "ready", "running", "bound", "active", "succeeded":
		reason, message = pickCondition(conds, "Ready", "True", "Reconciled", "resource is ready")
		return reason, message, corev1.EventTypeNormal
	case "failed", "error":
		reason, message = pickCondition(conds, "Ready", "False", "Failed", "resource reconciliation failed")
		return reason, message, corev1.EventTypeWarning
	case "", "pending", "progressing", "creating", "updating", "deleting":
		// Skip — too noisy and usually superseded within a tick.
		return "", "", corev1.EventTypeNormal
	default:
		return "Phase" + phase, "phase=" + phase, corev1.EventTypeNormal
	}
}

// pickCondition returns the reason/message of the first condition whose type
// matches typeName and (when wantStatus != "") whose status matches wantStatus.
// Falls back to defaultReason/defaultMessage when nothing matches.
func pickCondition(conds []interface{}, typeName, wantStatus, defaultReason, defaultMessage string) (string, string) {
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != typeName {
			continue
		}
		if wantStatus != "" {
			if s, _ := m["status"].(string); s != wantStatus {
				continue
			}
		}
		r, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		if r == "" {
			r = defaultReason
		}
		if msg == "" {
			msg = defaultMessage
		}
		return r, msg
	}
	return defaultReason, defaultMessage
}
