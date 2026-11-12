// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	leaseKind      = "Lease"
	leaseNamespace = "kognition-system"
)

// LeaderElectionConfig configures leader election behaviour.
type LeaderElectionConfig struct {
	// LeaseName identifies the lock (e.g., "controller-manager").
	LeaseName string

	// LeaseDuration is how long a lease is valid before expiration.
	LeaseDuration time.Duration

	// RenewDeadline is the interval at which the leader renews its lease.
	// Must be less than LeaseDuration.
	RenewDeadline time.Duration

	// RetryPeriod is how often non-leaders attempt to acquire the lease.
	RetryPeriod time.Duration

	// Identity is a unique identifier for this instance. Auto-generated if empty.
	Identity string
}

func (c *LeaderElectionConfig) setDefaults() {
	if c.LeaseDuration == 0 {
		c.LeaseDuration = 15 * time.Second
	}
	if c.RenewDeadline == 0 {
		c.RenewDeadline = 10 * time.Second
	}
	if c.RetryPeriod == 0 {
		c.RetryPeriod = 2 * time.Second
	}
	if c.Identity == "" {
		c.Identity = uuid.New().String()
	}
}

// leaseRecord is the JSON body stored in the resources table.
type leaseRecord struct {
	HolderIdentity  string `json:"holderIdentity"`
	LeaseDuration   string `json:"leaseDuration"`
	AcquireTime     string `json:"acquireTime"`
	RenewTime       string `json:"renewTime"`
	ResourceVersion int64  `json:"resourceVersion"`
}

// LeaderElector uses the store's optimistic concurrency to implement leader election.
type LeaderElector struct {
	cfg    LeaderElectionConfig
	store  store.Store
	logger *zap.Logger
}

// NewLeaderElector creates a new leader elector.
func NewLeaderElector(cfg LeaderElectionConfig, s store.Store, logger *zap.Logger) *LeaderElector {
	cfg.setDefaults()
	return &LeaderElector{
		cfg:    cfg,
		store:  s,
		logger: logger.Named("leader-election"),
	}
}

// Run attempts to acquire and hold the leader lease, calling onStartedLeading
// when elected. When the lease is lost (or ctx is canceled), onStartedLeading's
// context is canceled. Blocks until ctx is canceled.
func (le *LeaderElector) Run(ctx context.Context, onStartedLeading func(ctx context.Context)) error {
	le.logger.Info("starting leader election",
		zap.String("identity", le.cfg.Identity),
		zap.String("lease", le.cfg.LeaseName),
	)

	for {
		// Try to acquire the lease.
		if le.tryAcquire(ctx) {
			le.logger.Info("acquired leadership", zap.String("identity", le.cfg.Identity))

			// Run as leader with a cancelable context.
			leaderCtx, leaderCancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				onStartedLeading(leaderCtx)
				close(done)
			}()

			// Renew the lease until we lose it or ctx is canceled.
			le.renewLoop(ctx, leaderCancel)

			// Wait for the callback to finish after cancel.
			<-done
			le.logger.Info("lost leadership", zap.String("identity", le.cfg.Identity))
		}

		if ctx.Err() != nil {
			return nil
		}

		// Wait before retrying.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(le.cfg.RetryPeriod):
		}
	}
}

// tryAcquire attempts to create or take over the lease. Returns true if we became the leader.
func (le *LeaderElector) tryAcquire(ctx context.Context) bool {
	now := time.Now().UTC()
	record := leaseRecord{
		HolderIdentity: le.cfg.Identity,
		LeaseDuration:  le.cfg.LeaseDuration.String(),
		AcquireTime:    now.Format(time.RFC3339),
		RenewTime:      now.Format(time.RFC3339),
	}

	// Try to create the lease (first-ever acquisition).
	data, _ := json.Marshal(wrapLease(le.cfg.LeaseName, record))
	err := le.store.Create(ctx, leaseKind, leaseNamespace, le.cfg.LeaseName, data)
	if err == nil {
		return true
	}
	if !errors.Is(err, store.ErrAlreadyExists) {
		le.logger.Warn("failed to create lease", zap.Error(err))
		return false
	}

	// Lease exists — check if it's expired.
	existing, err := le.store.Get(ctx, leaseKind, leaseNamespace, le.cfg.LeaseName)
	if err != nil {
		le.logger.Warn("failed to get existing lease", zap.Error(err))
		return false
	}

	var wrapper leaseWrapper
	if err := json.Unmarshal(existing, &wrapper); err != nil {
		le.logger.Warn("failed to parse existing lease", zap.Error(err))
		return false
	}

	existingRecord := wrapper.Spec

	// Re-acquisition after restart must durably refresh the lease before this
	// process starts leading. renew also rechecks ownership and version.
	if existingRecord.HolderIdentity == le.cfg.Identity {
		return le.renew(ctx)
	}

	// Check if the existing lease is expired.
	renewTime, err := time.Parse(time.RFC3339, existingRecord.RenewTime)
	if err != nil {
		le.logger.Warn("failed to parse renew time", zap.Error(err))
		return false
	}

	leaseDur, err := time.ParseDuration(existingRecord.LeaseDuration)
	if err != nil {
		leaseDur = le.cfg.LeaseDuration
	}

	if now.Before(renewTime.Add(leaseDur)) {
		// Lease is still valid, someone else is the leader.
		return false
	}

	// Lease is expired — attempt to take over with optimistic concurrency.
	record.AcquireTime = now.Format(time.RFC3339)
	record.RenewTime = now.Format(time.RFC3339)
	data, _ = json.Marshal(wrapLease(le.cfg.LeaseName, record))

	err = le.store.Update(ctx, leaseKind, leaseNamespace, le.cfg.LeaseName, data, wrapper.Metadata.ResourceVersion)
	if err != nil {
		le.logger.Warn("failed to take over expired lease", zap.Error(err))
		return false
	}

	return true
}

// renewLoop renews the lease on a ticker. Calls cancel() if renewal fails.
func (le *LeaderElector) renewLoop(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(le.cfg.RenewDeadline)
	defer ticker.Stop()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !le.renew(ctx) {
				le.logger.Warn("failed to renew lease, stepping down")
				return
			}
		}
	}
}

// renew updates the lease's renewTime. Returns false if the renewal fails.
func (le *LeaderElector) renew(ctx context.Context) bool {
	existing, err := le.store.Get(ctx, leaseKind, leaseNamespace, le.cfg.LeaseName)
	if err != nil {
		return false
	}

	var wrapper leaseWrapper
	if err := json.Unmarshal(existing, &wrapper); err != nil {
		return false
	}

	// Verify we're still the holder.
	if wrapper.Spec.HolderIdentity != le.cfg.Identity {
		return false
	}

	now := time.Now().UTC()
	wrapper.Spec.RenewTime = now.Format(time.RFC3339)
	data, _ := json.Marshal(wrapper)

	err = le.store.Update(ctx, leaseKind, leaseNamespace, le.cfg.LeaseName, data, wrapper.Metadata.ResourceVersion)
	return err == nil
}

// leaseWrapper wraps the lease record in the standard resource envelope.
type leaseWrapper struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		ResourceVersion int64  `json:"resourceVersion"`
	} `json:"metadata"`
	Spec leaseRecord `json:"spec"`
}

func wrapLease(name string, rec leaseRecord) leaseWrapper {
	return leaseWrapper{
		APIVersion: "kognition.dev/v1alpha1",
		Kind:       leaseKind,
		Metadata: struct {
			Namespace       string `json:"namespace"`
			Name            string `json:"name"`
			ResourceVersion int64  `json:"resourceVersion"`
		}{
			Namespace: leaseNamespace,
			Name:      name,
		},
		Spec: rec,
	}
}
