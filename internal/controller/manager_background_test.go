// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

type funcRunnable func(ctx context.Context) error

func (f funcRunnable) Start(ctx context.Context) error { return f(ctx) }

// orderLog records the order in which things happened.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *orderLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func startManager(t *testing.T, mgr *Manager, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = mgr.Start(ctx)
		close(done)
	}()
	return done
}

func TestManager_BackgroundRunnablesOutliveControllersAndRunnables(t *testing.T) {
	mgr := NewManager(ManagerConfig{Store: newMemStore(), Logger: zaptest.NewLogger(t)})
	log := &orderLog{}
	backgroundCtx := make(chan context.Context, 1)
	backgroundStarted := make(chan struct{})
	var backgroundErrWhileDraining error

	mgr.AddRunnable(funcRunnable(func(ctx context.Context) error {
		<-ctx.Done()
		// Draining: work that is still finishing, such as a model call in flight.
		time.Sleep(50 * time.Millisecond)
		backgroundErrWhileDraining = (<-backgroundCtx).Err()
		log.add("runnable drained")
		return nil
	}))
	mgr.AddBackgroundRunnable(funcRunnable(func(ctx context.Context) error {
		backgroundCtx <- ctx
		close(backgroundStarted)
		<-ctx.Done()
		log.add("background stopped")
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := startManager(t, mgr, ctx)
	waitForTestSignal(t, backgroundStarted, "background runnable start")
	cancel()
	waitForTestSignal(t, done, "manager shutdown")

	if got := log.all(); len(got) != 2 || got[0] != "runnable drained" || got[1] != "background stopped" {
		t.Fatalf("order = %v, want the background runnable to stop only after the runnable drained", got)
	}
	if backgroundErrWhileDraining != nil {
		t.Fatalf("background context error while the runnable drained = %v, want it still alive", backgroundErrWhileDraining)
	}
}

func TestManager_BackgroundRunnablesKeepTheContextValuesAndAreCanceledAtTheEnd(t *testing.T) {
	type key struct{}
	mgr := NewManager(ManagerConfig{Store: newMemStore(), Logger: zaptest.NewLogger(t)})
	started := make(chan struct{})
	var value any
	var canceled error
	mgr.AddBackgroundRunnable(funcRunnable(func(ctx context.Context) error {
		value = ctx.Value(key{})
		close(started)
		<-ctx.Done()
		canceled = ctx.Err()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "manager-scoped"))
	done := startManager(t, mgr, ctx)
	waitForTestSignal(t, started, "background runnable start")
	cancel()
	waitForTestSignal(t, done, "manager shutdown")

	if value != "manager-scoped" {
		t.Fatalf("context value = %v, want the manager context's values kept", value)
	}
	if !errors.Is(canceled, context.Canceled) {
		t.Fatalf("context error at the end = %v, want it canceled once the manager is done", canceled)
	}
}

func TestManager_ABackgroundRunnableFailureDoesNotStopTheManager(t *testing.T) {
	mgr := NewManager(ManagerConfig{Store: newMemStore(), Logger: zaptest.NewLogger(t)})
	failed := make(chan struct{})
	mgr.AddBackgroundRunnable(funcRunnable(func(context.Context) error {
		close(failed)
		return errors.New("publish failed")
	}))
	survived := make(chan struct{})
	mgr.AddRunnable(funcRunnable(func(ctx context.Context) error {
		<-failed
		close(survived)
		<-ctx.Done()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := startManager(t, mgr, ctx)
	waitForTestSignal(t, survived, "runnable after a background failure")
	cancel()
	waitForTestSignal(t, done, "manager shutdown")
}
