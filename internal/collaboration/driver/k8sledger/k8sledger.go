// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package k8sledger stores a collaboration ledger and the remote A2A task
// handles of its dispatches in ConfigMaps. Ledger compare-and-swap uses the
// ConfigMap resourceVersion, so the API server arbitrates concurrent writers.
package k8sledger

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
)

const (
	// StateKey is the ConfigMap data key that holds the encoded ledger.
	StateKey = "state"
	// HandlesSuffix names the handle ConfigMap after the ledger ConfigMap.
	HandlesSuffix = "-handles"

	maxHandleAttempts = 5
	maxHandleLen      = 256
)

var handleKey = regexp.MustCompile(`^[A-Za-z0-9][-._A-Za-z0-9]{0,252}$`)

// Config identifies one activation's ledger objects.
type Config struct {
	ConfigMaps corev1client.ConfigMapInterface
	// Name is the ledger ConfigMap name; handles use Name+HandlesSuffix.
	Name string
	// Labels and Owners are applied to every object so they are garbage
	// collected with their owning workflow run.
	Labels map[string]string
	Owners []metav1.OwnerReference
}

func (c Config) validate() error {
	if c.ConfigMaps == nil {
		return errors.New("k8sledger: a ConfigMap client is required")
	}
	if errs := validation.IsDNS1123Subdomain(c.Name + HandlesSuffix); len(errs) > 0 {
		return fmt.Errorf("k8sledger: invalid name %q: %v", c.Name, errs)
	}
	return nil
}

func (c Config) object(name string) *corev1.ConfigMap {
	labels := make(map[string]string, len(c.Labels))
	for k, v := range c.Labels {
		labels[k] = v
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels,
		OwnerReferences: append([]metav1.OwnerReference(nil), c.Owners...)}}
}

// Ledger is a driver.Ledger backed by one ConfigMap.
type Ledger struct{ cfg Config }

var _ driver.Ledger = (*Ledger)(nil)

// NewLedger validates the configuration.
func NewLedger(cfg Config) (*Ledger, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Ledger{cfg: cfg}, nil
}

// Load implements driver.Ledger.
func (l *Ledger) Load(ctx context.Context) (collaboration.State, driver.Version, error) {
	cm, err := l.cfg.ConfigMaps.Get(ctx, l.cfg.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return collaboration.State{}, "", driver.ErrNotFound
	}
	if err != nil {
		return collaboration.State{}, "", err
	}
	raw, ok := cm.Data[StateKey]
	if !ok {
		return collaboration.State{}, "", errors.New("k8sledger: ledger object has no state")
	}
	st, err := collaboration.Decode([]byte(raw))
	if err != nil {
		return collaboration.State{}, "", err
	}
	return st, driver.Version(cm.ResourceVersion), nil
}

// Create implements driver.Ledger.
func (l *Ledger) Create(ctx context.Context, st collaboration.State) (driver.Version, error) {
	raw, err := collaboration.Encode(st)
	if err != nil {
		return "", err
	}
	cm := l.cfg.object(l.cfg.Name)
	cm.Data = map[string]string{StateKey: string(raw)}
	out, err := l.cfg.ConfigMaps.Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return "", driver.ErrExists
	}
	if err != nil {
		return "", err
	}
	return driver.Version(out.ResourceVersion), nil
}

// Save implements driver.Ledger.
func (l *Ledger) Save(ctx context.Context, st collaboration.State, expect driver.Version) (driver.Version, error) {
	if expect == "" {
		return "", driver.ErrConflict
	}
	raw, err := collaboration.Encode(st)
	if err != nil {
		return "", err
	}
	cm := l.cfg.object(l.cfg.Name)
	cm.ResourceVersion = string(expect)
	cm.Data = map[string]string{StateKey: string(raw)}
	out, err := l.cfg.ConfigMaps.Update(ctx, cm, metav1.UpdateOptions{})
	switch {
	case apierrors.IsConflict(err):
		return "", driver.ErrConflict
	case apierrors.IsNotFound(err):
		return "", driver.ErrNotFound
	case err != nil:
		return "", err
	}
	return driver.Version(out.ResourceVersion), nil
}

// Handles stores remote task handles so a restarted controller can still
// observe and cancel dispatches that were already contacted. It satisfies
// a2atransport.Handles.
type Handles struct{ cfg Config }

// NewHandles validates the configuration.
func NewHandles(cfg Config) (*Handles, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Handles{cfg: cfg}, nil
}

func (h *Handles) name() string { return h.cfg.Name + HandlesSuffix }

// Put records the remote task of a dispatch. Re-recording the same task is a
// no-op; recording a different task for the same dispatch is an error.
func (h *Handles) Put(ctx context.Context, dispatch, remoteTask string) error {
	if !handleKey.MatchString(dispatch) || remoteTask == "" || len(remoteTask) > maxHandleLen {
		return errors.New("k8sledger: invalid handle")
	}
	for attempt := 0; attempt < maxHandleAttempts; attempt++ {
		cm, err := h.cfg.ConfigMaps.Get(ctx, h.name(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh := h.cfg.object(h.name())
			fresh.Data = map[string]string{dispatch: remoteTask}
			_, err = h.cfg.ConfigMaps.Create(ctx, fresh, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		if cur, ok := cm.Data[dispatch]; ok {
			if cur == remoteTask {
				return nil
			}
			return fmt.Errorf("k8sledger: dispatch %q already has a different remote task", dispatch)
		}
		next := cm.DeepCopy()
		if next.Data == nil {
			next.Data = map[string]string{}
		}
		next.Data[dispatch] = remoteTask
		if _, err = h.cfg.ConfigMaps.Update(ctx, next, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return driver.ErrConflict
}

// Replace moves the handle of a dispatch from expected to next. It is a no-op
// when next is already recorded and fails with a2atransport.ErrHandleChanged
// when anything else is.
func (h *Handles) Replace(ctx context.Context, dispatch, expected, next string) error {
	if !handleKey.MatchString(dispatch) || expected == "" || next == "" || len(next) > maxHandleLen {
		return errors.New("k8sledger: invalid handle")
	}
	for attempt := 0; attempt < maxHandleAttempts; attempt++ {
		cm, err := h.cfg.ConfigMaps.Get(ctx, h.name(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return a2atransport.ErrHandleChanged
			}
			return err
		}
		cur, ok := cm.Data[dispatch]
		switch {
		case ok && cur == next:
			return nil
		case !ok || cur != expected:
			return a2atransport.ErrHandleChanged
		}
		updated := cm.DeepCopy()
		updated.Data[dispatch] = next
		if _, err = h.cfg.ConfigMaps.Update(ctx, updated, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return driver.ErrConflict
}

// Get returns the remote task recorded for a dispatch.
func (h *Handles) Get(ctx context.Context, dispatch string) (string, bool, error) {
	cm, err := h.cfg.ConfigMaps.Get(ctx, h.name(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	v, ok := cm.Data[dispatch]
	return v, ok, nil
}
