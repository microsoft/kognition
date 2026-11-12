// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

// Manager hosts and runs all controllers with optional leader election.
type Manager struct {
	logger      *zap.Logger
	store       store.Store
	controllers []*Controller
	runnables   []Runnable
	informers   map[string]*Informer // kind -> shared informer
	election    *LeaderElector
	mu          sync.Mutex
}

// Runnable is background work scoped to the manager's leadership lifecycle.
type Runnable interface {
	Start(context.Context) error
}

// ManagerConfig holds Manager construction parameters.
type ManagerConfig struct {
	Store          store.Store
	Logger         *zap.Logger
	LeaderElection *LeaderElectionConfig // nil = no leader election
}

// NewManager creates a new controller manager.
func NewManager(cfg ManagerConfig) *Manager {
	m := &Manager{
		logger:    cfg.Logger,
		store:     cfg.Store,
		informers: make(map[string]*Informer),
	}
	if cfg.LeaderElection != nil {
		m.election = NewLeaderElector(*cfg.LeaderElection, cfg.Store, cfg.Logger)
	}
	return m
}

// Register adds a controller to the manager. It creates a shared informer
// for the controller's kind if one does not already exist.
func (m *Manager) Register(cfg ControllerConfig) *Controller {
	m.mu.Lock()
	defer m.mu.Unlock()

	informer, ok := m.informers[cfg.Kind]
	if !ok {
		informer = NewInformer(m.store, cfg.Kind, m.logger.Named("informer"))
		m.informers[cfg.Kind] = informer
	}

	c := NewController(cfg, informer, m.logger)
	m.controllers = append(m.controllers, c)
	return c
}

// AddRunnable registers background work that starts and stops with controllers.
func (m *Manager) AddRunnable(runnable Runnable) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runnables = append(m.runnables, runnable)
}

// AddEventHandler registers an additional handler on an already-registered
// kind's shared informer. It supports cross-kind dependency invalidation.
func (m *Manager) AddEventHandler(kind string, handler EventHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	informer, ok := m.informers[kind]
	if !ok {
		return fmt.Errorf("informer for kind %q is not registered", kind)
	}
	informer.AddEventHandler(handler)
	return nil
}

// Start begins all controllers. If leader election is configured, controllers
// only run while this instance is the leader. Blocks until ctx is canceled.
func (m *Manager) Start(ctx context.Context) error {
	m.logger.Info("starting controller manager",
		zap.Int("controllers", len(m.controllers)),
		zap.Int("runnables", len(m.runnables)),
		zap.Bool("leaderElection", m.election != nil),
	)

	if m.election != nil {
		return m.election.Run(ctx, func(ctx context.Context) {
			m.logger.Info("elected as leader, starting controllers")
			m.runControllers(ctx)
		})
	}

	m.runControllers(ctx)
	return nil
}

// runControllers starts all controllers and blocks until ctx is done.
func (m *Manager) runControllers(ctx context.Context) {
	var wg sync.WaitGroup

	for _, c := range m.controllers {
		wg.Add(1)
		go func(ctrl *Controller) {
			defer wg.Done()
			if err := ctrl.Run(ctx); err != nil {
				m.logger.Error("controller error",
					zap.String("controller", ctrl.name),
					zap.Error(err),
				)
			}
		}(c)
	}
	for _, runnable := range m.runnables {
		wg.Add(1)
		go func(r Runnable) {
			defer wg.Done()
			if err := r.Start(ctx); err != nil {
				m.logger.Error("manager runnable error", zap.Error(err))
			}
		}(runnable)
	}

	<-ctx.Done()
	m.logger.Info("shutting down controller manager")
	wg.Wait()
}
