// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// TestWaitForShutdownManagerCanceledIsClean covers the branch where the
// controller-runtime manager exits with context.Canceled: waitForShutdown must
// treat this as a graceful shutdown (no error).
func TestWaitForShutdownManagerCanceledIsClean(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerErrors <- context.Canceled

	if err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors); err != nil {
		t.Fatalf("waitForShutdown returned %v, want nil", err)
	}
}

// TestWaitForShutdownManagerErrorIsWrapped ensures a manager failure other
// than context.Canceled propagates back to the caller so run() can log and
// exit non-zero.
func TestWaitForShutdownManagerErrorIsWrapped(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fatal := errors.New("informer boom")
	managerErrors <- fatal

	err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors)
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("waitForShutdown err = %v, want wrapping %v", err, fatal)
	}
	if !strings.Contains(err.Error(), "controller-runtime manager exited") {
		t.Fatalf("waitForShutdown err = %q, missing manager tag", err)
	}
}

// TestWaitForShutdownGrpcServerStoppedIsClean covers the branch where the
// pluginsdk gRPC server has already been asked to stop (grpc.ErrServerStopped)
// and must be treated as a graceful exit.
func TestWaitForShutdownGrpcServerStoppedIsClean(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	grpcErrors <- grpc.ErrServerStopped

	if err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors); err != nil {
		t.Fatalf("waitForShutdown returned %v, want nil", err)
	}
}

// TestWaitForShutdownGrpcErrorIsWrapped covers unexpected gRPC exits.
func TestWaitForShutdownGrpcErrorIsWrapped(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fatal := errors.New("serve exploded")
	grpcErrors <- fatal

	err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors)
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("waitForShutdown err = %v, want wrapping %v", err, fatal)
	}
	if !strings.Contains(err.Error(), "knowledge gRPC server exited") {
		t.Fatalf("waitForShutdown err = %q, missing gRPC tag", err)
	}
}

// TestWaitForShutdownAdminClosedIsClean covers the admin HTTP server exiting
// with http.ErrServerClosed (deferred Shutdown in run()).
func TestWaitForShutdownAdminClosedIsClean(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adminErrors <- http.ErrServerClosed

	if err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors); err != nil {
		t.Fatalf("waitForShutdown returned %v, want nil", err)
	}
}

// TestWaitForShutdownAdminErrorIsWrapped covers unexpected admin HTTP exits
// (e.g. bind failure during startup).
func TestWaitForShutdownAdminErrorIsWrapped(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fatal := errors.New("admin bind failed")
	adminErrors <- fatal

	err := waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors)
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("waitForShutdown err = %v, want wrapping %v", err, fatal)
	}
	if !strings.Contains(err.Error(), "admin HTTP server exited") {
		t.Fatalf("waitForShutdown err = %q, missing admin tag", err)
	}
}

// TestWaitForShutdownRespondsToContextCancel covers the signal-handling
// branch: when the parent context is canceled (SIGTERM in production),
// waitForShutdown must GracefulStop the gRPC server and return nil.
func TestWaitForShutdownRespondsToContextCancel(t *testing.T) {
	managerErrors := make(chan error, 1)
	grpcErrors := make(chan error, 1)
	adminErrors := make(chan error, 1)
	server := grpc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before invoking waitForShutdown so the ctx.Done() branch is the
	// only ready case at the top-level select.
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- waitForShutdown(ctx, zap.NewNop(), server, managerErrors, grpcErrors, adminErrors)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForShutdown returned %v, want nil after context cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForShutdown did not return after context cancel; GracefulStop likely blocked")
	}
}
