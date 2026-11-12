// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"time"
)

// Request represents a reconciliation request.
type Request struct {
	// Namespace of the resource to reconcile.
	Namespace string

	// Name of the resource to reconcile.
	Name string

	// Kind of the resource.
	Kind string
}

// Result represents the outcome of a reconciliation.
type Result struct {
	// Requeue indicates the request should be re-enqueued after processing.
	Requeue bool

	// RequeueAfter requests re-enqueue after a specific duration.
	// Takes precedence over Requeue if non-zero.
	RequeueAfter time.Duration
}

// Reconciler is the interface that all controllers implement.
// The controller framework handles watching, queueing, and lifecycle;
// the reconciler only needs to implement the reconciliation logic.
type Reconciler interface {
	// Reconcile performs a single reconciliation pass for a resource.
	// Return a Result with Requeue or RequeueAfter to re-enqueue.
	// Return an error to trigger exponential backoff retry.
	Reconcile(ctx context.Context, req Request) (Result, error)
}
