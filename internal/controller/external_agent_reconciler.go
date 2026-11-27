// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	externalAgentKind = "ExternalAgent"

	externalAgentReadyRequeue    = 5 * time.Minute
	externalAgentNotReadyRequeue = time.Minute
)

// ExternalAgentCardObserver fetches the live Agent Card of an external agent and returns its digest. It must
// apply the same hardened transport rules as task delivery (TLS, no redirects, resolved-address checks).
type ExternalAgentCardObserver interface {
	ObserveCardDigest(ctx context.Context, agent v1alpha1.ExternalAgent) (string, error)
}

// ExternalAgentReconcilerOption configures an ExternalAgentReconciler.
type ExternalAgentReconcilerOption func(*ExternalAgentReconciler)

// WithExternalAgentEgressPolicy enables external agents and enforces the operator egress allow-list. An empty
// list permits nothing. Without this option the reconciler reports FeatureDisabled.
func WithExternalAgentEgressPolicy(allowList *externalagent.AllowList) ExternalAgentReconcilerOption {
	return func(r *ExternalAgentReconciler) {
		r.egressEnforced = true
		r.allowList = allowList
	}
}

// WithExternalAgentResolver replaces the DNS resolver.
func WithExternalAgentResolver(resolver externalagent.Resolver) ExternalAgentReconcilerOption {
	return func(r *ExternalAgentReconciler) { r.resolver = resolver }
}

// WithExternalAgentCardObserver enables live card verification. Without an observer the card and reachability
// conditions stay Unknown and the agent is never Ready.
func WithExternalAgentCardObserver(observer ExternalAgentCardObserver) ExternalAgentReconcilerOption {
	return func(r *ExternalAgentReconciler) { r.observer = observer }
}

// ExternalAgentReconciler reports whether an ExternalAgent is safe to use. It is the only writer of the
// ExternalAgent status. Task delivery re-checks every condition itself and never trusts this status.
type ExternalAgentReconciler struct {
	store          store.Store
	logger         *zap.Logger
	egressEnforced bool
	allowList      *externalagent.AllowList
	resolver       externalagent.Resolver
	observer       ExternalAgentCardObserver
}

// NewExternalAgentReconciler creates an ExternalAgentReconciler.
func NewExternalAgentReconciler(s store.Store, logger *zap.Logger, opts ...ExternalAgentReconcilerOption) *ExternalAgentReconciler {
	r := &ExternalAgentReconciler{
		store:    s,
		logger:   logger.With(zap.String("reconciler", "externalagent")),
		resolver: externalagent.DefaultResolver(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

type externalAgentCheck struct {
	status  lifecycle.ConditionStatus
	reason  string
	message string
}

// Reconcile implements Reconciler.
func (r *ExternalAgentReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, externalAgentKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get ExternalAgent: %w", err)
	}
	var agent v1alpha1.ExternalAgent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return Result{}, fmt.Errorf("unmarshal ExternalAgent: %w", err)
	}

	egress, reachable, pinned, observed, ready := r.evaluate(ctx, req.Namespace, agent)

	conditions := apiToLifecycleConditions(agent.Status.Conditions)
	for _, c := range []struct {
		conditionType string
		check         externalAgentCheck
	}{
		{v1alpha1.ExternalAgentConditionEgress, egress},
		{v1alpha1.ExternalAgentConditionReachable, reachable},
		{v1alpha1.ExternalAgentConditionCardPinned, pinned},
		{string(lifecycle.ConditionReady), ready},
	} {
		conditions = lifecycle.SetCondition(conditions, lifecycle.ConditionType(c.conditionType), c.check.status, c.check.reason, c.check.message)
	}
	desired := lifecycleToAPIConditions(conditions)

	if agent.Status.ObservedGeneration == agent.ObjectMeta.Generation &&
		agent.Status.ObservedCardDigest == observed &&
		reflect.DeepEqual(agent.Status.Conditions, desired) {
		return Result{RequeueAfter: externalAgentRequeue(ready)}, nil
	}

	agent.Status.ObservedGeneration = agent.ObjectMeta.Generation
	agent.Status.ObservedCardDigest = observed
	agent.Status.Conditions = desired
	data, err := json.Marshal(agent)
	if err != nil {
		return Result{}, fmt.Errorf("marshal ExternalAgent: %w", err)
	}
	if err := r.store.Update(ctx, externalAgentKind, req.Namespace, req.Name, data, agent.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update ExternalAgent status: %w", err)
	}
	r.logger.Info("ExternalAgent reconciled",
		zap.String("namespace", req.Namespace),
		zap.String("name", req.Name),
		zap.String("ready", string(ready.status)),
		zap.String("reason", ready.reason),
		requestid.ZapField(ctx))
	return Result{RequeueAfter: externalAgentRequeue(ready)}, nil
}

func externalAgentRequeue(ready externalAgentCheck) time.Duration {
	if ready.status == lifecycle.ConditionTrue {
		return externalAgentReadyRequeue
	}
	return externalAgentNotReadyRequeue
}

// evaluate runs the checks in dependency order. Each later check is Unknown when an earlier one failed, so a
// reader sees the first cause rather than a cascade.
func (r *ExternalAgentReconciler) evaluate(ctx context.Context, namespace string, agent v1alpha1.ExternalAgent) (egress, reachable, pinned externalAgentCheck, observed string, ready externalAgentCheck) {
	skipped := func(cause string) externalAgentCheck {
		return externalAgentCheck{lifecycle.ConditionUnknown, "NotEvaluated", cause}
	}
	notReady := func(reason, message string) externalAgentCheck {
		return externalAgentCheck{lifecycle.ConditionFalse, reason, message}
	}

	if err := agent.Spec.Validate(); err != nil {
		cause := err.Error()
		return skipped("spec is invalid"), skipped("spec is invalid"), skipped("spec is invalid"), "", notReady("InvalidSpec", cause)
	}
	host, port, err := v1alpha1.ParseExternalAgentEndpoint(agent.Spec.Endpoint)
	if err != nil {
		return skipped("endpoint is invalid"), skipped("endpoint is invalid"), skipped("endpoint is invalid"), "", notReady("InvalidSpec", err.Error())
	}

	if !r.egressEnforced {
		const disabled = "external agents are not enabled"
		return skipped(disabled), skipped(disabled), skipped(disabled), "", notReady("FeatureDisabled", disabled)
	}
	if !r.allowList.Permits(host, port) {
		message := fmt.Sprintf("%s:%d is not in the operator egress allow-list", host, port)
		egress = externalAgentCheck{lifecycle.ConditionFalse, "NotAllowListed", message}
		return egress, skipped("egress is not allowed"), skipped("egress is not allowed"), "", notReady("EgressNotAllowed", message)
	}
	egress = externalAgentCheck{lifecycle.ConditionTrue, "Allowed", "endpoint is permitted by the operator egress policy"}

	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := externalagent.ResolvePermitted(lookup, r.resolver, host); err != nil {
		reason := "Unresolvable"
		if errors.Is(err, externalagent.ErrForbiddenAddress) {
			reason = "ForbiddenAddress"
			egress = externalAgentCheck{lifecycle.ConditionFalse, reason, err.Error()}
		}
		return egress, externalAgentCheck{lifecycle.ConditionFalse, reason, err.Error()}, skipped("endpoint is not reachable"), "", notReady("EndpointUnavailable", err.Error())
	}

	if _, err := r.store.Get(ctx, "SecretRef", namespace, agent.Spec.CredentialRef.Name); err != nil {
		message := fmt.Sprintf("credentialRef %q could not be read", agent.Spec.CredentialRef.Name)
		if errors.Is(err, store.ErrNotFound) {
			message = fmt.Sprintf("credentialRef %q does not exist", agent.Spec.CredentialRef.Name)
		}
		return egress, skipped("credential is unavailable"), skipped("credential is unavailable"), "", notReady("CredentialUnavailable", message)
	}

	if r.observer == nil {
		message := "live card verification is not configured"
		return egress, externalAgentCheck{lifecycle.ConditionUnknown, "NotObserved", message}, externalAgentCheck{lifecycle.ConditionUnknown, "NotObserved", message}, "", notReady("CardNotVerified", message)
	}
	digest, err := r.observer.ObserveCardDigest(ctx, agent)
	if err != nil {
		return egress, externalAgentCheck{lifecycle.ConditionFalse, "CardUnavailable", err.Error()}, skipped("the card could not be fetched"), "", notReady("EndpointUnavailable", err.Error())
	}
	reachable = externalAgentCheck{lifecycle.ConditionTrue, "CardFetched", "the Agent Card was fetched from the endpoint"}
	if digest != agent.Spec.Card.Digest {
		message := fmt.Sprintf("the served Agent Card digest %s does not match the pinned digest", digest)
		return egress, reachable, externalAgentCheck{lifecycle.ConditionFalse, "DigestMismatch", message}, digest, notReady("CardDigestMismatch", message)
	}
	pinned = externalAgentCheck{lifecycle.ConditionTrue, "DigestMatches", "the served Agent Card matches the pinned digest"}
	return egress, reachable, pinned, digest, externalAgentCheck{lifecycle.ConditionTrue, "Ready", "the external agent is verified and may be used"}
}
