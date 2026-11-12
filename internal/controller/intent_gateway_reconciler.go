// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// IntentGatewayFinalizer is the finalizer applied to IntentGateway resources.
	IntentGatewayFinalizer = "kognition.dev/intent-gateway-cleanup"

	// intentGatewayKind is the kind string for IntentGateway resources.
	intentGatewayKind = "IntentGateway"
)

// IntentGatewayReconciler reconciles IntentGateway resources by validating
// that all referenced route targets exist and setting status accordingly.
type IntentGatewayReconciler struct {
	store             store.Store
	logger            *zap.Logger
	servingEndpointFn func(context.Context, *v1alpha1.IntentGateway) (string, bool)
}

// IntentGatewayReconcilerOption configures an IntentGateway reconciler.
type IntentGatewayReconcilerOption func(*IntentGatewayReconciler)

// WithIntentGatewayServingAvailable controls whether a reconciled gateway can
// advertise its goal submission endpoint.
func WithIntentGatewayServingAvailable(available bool) IntentGatewayReconcilerOption {
	return func(reconciler *IntentGatewayReconciler) {
		if available {
			reconciler.servingEndpointFn = func(_ context.Context, gateway *v1alpha1.IntentGateway) (string, bool) {
				return intentGatewayEndpoint(gateway), true
			}
			return
		}
		reconciler.servingEndpointFn = func(context.Context, *v1alpha1.IntentGateway) (string, bool) { return "", false }
	}
}

// WithIntentGatewayServingEndpointProvider makes readiness and endpoint
// publication reflect an external serving workload's current availability.
func WithIntentGatewayServingEndpointProvider(provider func(context.Context, *v1alpha1.IntentGateway) (string, bool)) IntentGatewayReconcilerOption {
	return func(reconciler *IntentGatewayReconciler) {
		reconciler.servingEndpointFn = provider
	}
}

// IntentGatewayDependencyHandler requeues gateways that reference a changed
// Agent, AgentService, or Workflow. Standalone informers otherwise watch only
// their own kind.
type IntentGatewayDependencyHandler struct {
	store      store.Store
	targetKind string
	target     EventHandler
	logger     *zap.Logger
}

func NewIntentGatewayDependencyHandler(st store.Store, targetKind string, target EventHandler, logger *zap.Logger) *IntentGatewayDependencyHandler {
	return &IntentGatewayDependencyHandler{store: st, targetKind: targetKind, target: target, logger: logger.Named("intent-gateway-dependency")}
}

func (h *IntentGatewayDependencyHandler) OnAdd(key string)    { h.enqueueDependents(key) }
func (h *IntentGatewayDependencyHandler) OnUpdate(key string) { h.enqueueDependents(key) }
func (h *IntentGatewayDependencyHandler) OnDelete(key string) { h.enqueueDependents(key) }

func (h *IntentGatewayDependencyHandler) enqueueDependents(key string) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return
	}
	namespace, targetName := parts[0], parts[1]
	rawGateways, err := h.store.List(context.Background(), intentGatewayKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		h.logger.Error("list intent gateways for dependency event", zap.Error(err))
		return
	}
	for _, raw := range rawGateways {
		var gateway v1alpha1.IntentGateway
		if err := json.Unmarshal(raw, &gateway); err != nil {
			continue
		}
		for _, route := range gateway.Spec.Routes {
			if route.TargetKind == h.targetKind && route.TargetRef.Name == targetName {
				h.target.OnUpdate(KeyFunc(namespace, gateway.ObjectMeta.Name))
				break
			}
		}
	}
}

// NewIntentGatewayReconciler creates a new IntentGatewayReconciler.
func NewIntentGatewayReconciler(s store.Store, logger *zap.Logger, options ...IntentGatewayReconcilerOption) *IntentGatewayReconciler {
	reconciler := &IntentGatewayReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "intent-gateway")),
		servingEndpointFn: func(_ context.Context, gateway *v1alpha1.IntentGateway) (string, bool) {
			return intentGatewayEndpoint(gateway), true
		},
	}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

// Reconcile implements the Reconciler interface for IntentGateway resources.
func (r *IntentGatewayReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the IntentGateway from the store.
	raw, err := r.store.Get(ctx, intentGatewayKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("intent gateway not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get intent gateway: %w", err)
	}

	var gw v1alpha1.IntentGateway
	if err := json.Unmarshal(raw, &gw); err != nil {
		return Result{}, fmt.Errorf("unmarshal intent gateway: %w", err)
	}

	// Handle deletion: remove finalizer.
	if gw.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &gw, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(gw.ObjectMeta.Finalizers, IntentGatewayFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(gw.ObjectMeta.Finalizers, IntentGatewayFinalizer)
		gw.ObjectMeta.Finalizers = finalizers
		if err := r.updateGateway(ctx, &gw); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}
	if err := validateIntentGatewayContract(&gw); err != nil {
		if setErr := r.setNotReady(ctx, &gw, "InvalidGatewayContract", err.Error()); setErr != nil {
			return Result{}, setErr
		}
		return Result{}, nil
	}

	// Validate all routes: ensure each target reference exists.
	for _, route := range gw.Spec.Routes {
		if err := r.validateRoute(ctx, &gw, route); err != nil {
			log.Warn("route validation failed", zap.String("pattern", route.Pattern), zap.Error(err))
			if setErr := r.setNotReady(ctx, &gw, "RouteTargetNotFound", err.Error()); setErr != nil {
				return Result{}, setErr
			}
			return Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	endpoint, servingAvailable := r.servingEndpointFn(ctx, &gw)
	if !servingAvailable {
		if err := r.setNotReady(ctx, &gw, "ServingUnavailable", "goal endpoint serving is not available in this deployment mode"); err != nil {
			return Result{}, fmt.Errorf("set serving unavailable status: %w", err)
		}
		return Result{}, nil
	}

	// All routes valid — set status to Ready.
	if gw.Status.Phase == v1alpha1.PhaseReady && gw.Status.Endpoint == endpoint {
		return Result{}, nil
	}
	if err := r.updateStatus(ctx, &gw, endpoint); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	return Result{}, nil
}

// handleDeletion removes the finalizer and persists the update.
func (r *IntentGatewayReconciler) handleDeletion(ctx context.Context, gw *v1alpha1.IntentGateway, log *zap.Logger) (Result, error) {
	log.Info("handling intent gateway deletion")

	finalizers, removed := lifecycle.RemoveFinalizer(gw.ObjectMeta.Finalizers, IntentGatewayFinalizer)
	if removed {
		gw.ObjectMeta.Finalizers = finalizers
		if err := r.updateGateway(ctx, gw); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return Result{}, nil
}

// validateRoute checks that a route's target exists in the store.
func (r *IntentGatewayReconciler) validateRoute(ctx context.Context, gw *v1alpha1.IntentGateway, route v1alpha1.IntentRoute) error {
	pattern := strings.TrimSpace(route.Pattern)
	if pattern == "" {
		return fmt.Errorf("route pattern is required")
	}
	if strings.ContainsAny(pattern, "*?[") {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("route %q: invalid pattern: %w", route.Pattern, err)
		}
	}
	targetName := route.TargetRef.Name
	if targetName == "" {
		return fmt.Errorf("route %q: targetRef.name is required", route.Pattern)
	}

	targetKind := route.TargetKind
	if targetKind != "Agent" && targetKind != "AgentService" && targetKind != "Workflow" {
		return fmt.Errorf("route %q: unsupported targetKind %q (must be Agent, AgentService, or Workflow)", route.Pattern, targetKind)
	}

	_, err := r.store.Get(ctx, targetKind, gw.ObjectMeta.Namespace, targetName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("route %q: %s %q not found", route.Pattern, targetKind, targetName)
		}
		return fmt.Errorf("route %q: get %s %q: %w", route.Pattern, targetKind, targetName, err)
	}
	return nil
}

// updateStatus sets the IntentGateway status to Ready with appropriate conditions.
func (r *IntentGatewayReconciler) updateStatus(ctx context.Context, gw *v1alpha1.IntentGateway, endpoint string) error {
	gw.Status.Phase = v1alpha1.PhaseReady
	gw.Status.Endpoint = endpoint
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(gw.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"RoutesValid",
		fmt.Sprintf("%d routes validated", len(gw.Spec.Routes)),
	)
	gw.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	return r.updateGateway(ctx, gw)
}

// setNotReady sets a not-ready condition and persists the status.
func (r *IntentGatewayReconciler) setNotReady(ctx context.Context, gw *v1alpha1.IntentGateway, reason, message string) error {
	gw.Status.Phase = v1alpha1.PhasePending
	gw.Status.Endpoint = ""
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(gw.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	)
	gw.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	return r.updateGateway(ctx, gw)
}

func validateIntentGatewayContract(gateway *v1alpha1.IntentGateway) error {
	if gateway.Spec.Protocol != "" && gateway.Spec.Protocol != "http" {
		return fmt.Errorf("protocol %q is unsupported; only http is available", gateway.Spec.Protocol)
	}
	auth := gateway.Spec.AuthConfig
	if auth == nil || auth.Mode == "" || auth.Mode == "none" {
		return nil
	}
	if auth.Mode != "apiKey" {
		return fmt.Errorf("authConfig.mode %q is invalid (must be none or apiKey)", auth.Mode)
	}
	if auth.APIKeySecretRef == nil || strings.TrimSpace(auth.APIKeySecretRef.Name) == "" {
		return fmt.Errorf("authConfig.apiKeySecretRef.name is required when mode=apiKey")
	}
	if strings.ContainsAny(auth.Header, "\r\n") {
		return fmt.Errorf("authConfig.header must not contain a newline")
	}
	return nil
}

func intentGatewayEndpoint(gateway *v1alpha1.IntentGateway) string {
	return IntentGatewayGoalPath(gateway.ObjectMeta.Namespace, gateway.ObjectMeta.Name)
}

// IntentGatewayGoalPath returns the stable HTTP path for a gateway goal endpoint.
func IntentGatewayGoalPath(namespace, name string) string {
	return fmt.Sprintf("/apis/kognition.dev/v1alpha1/namespaces/%s/intentgateways/%s/goals", namespace, name)
}

// updateGateway persists the current IntentGateway state back to the store.
func (r *IntentGatewayReconciler) updateGateway(ctx context.Context, gw *v1alpha1.IntentGateway) error {
	data, err := json.Marshal(gw)
	if err != nil {
		return fmt.Errorf("marshal intent gateway: %w", err)
	}
	return r.store.Update(ctx, intentGatewayKind, gw.ObjectMeta.Namespace, gw.ObjectMeta.Name, data, gw.ObjectMeta.ResourceVersion)
}
