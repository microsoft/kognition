// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

var errWatchResyncRequired = errors.New("watch resync required")

// resourceMeta is the minimal metadata extracted from a stored resource.
type resourceMeta struct {
	Metadata struct {
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		ResourceVersion int64  `json:"resourceVersion"`
	} `json:"metadata"`
}

// KeyFunc builds a queue key from namespace and name.
func KeyFunc(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// EventHandler receives notifications from an Informer.
type EventHandler interface {
	OnAdd(key string)
	OnUpdate(key string)
	OnDelete(key string)
}

// Informer watches a single resource kind via the store, maintains an
// in-memory cache, and dispatches change events to registered handlers.
type Informer struct {
	store  store.Store
	kind   string
	logger *zap.Logger

	handlersMu sync.RWMutex
	handlers   []EventHandler

	mu    sync.RWMutex
	cache map[string][]byte // key -> raw JSON

	synced atomic.Bool
}

// NewInformer creates an informer for the given resource kind.
func NewInformer(s store.Store, kind string, logger *zap.Logger) *Informer {
	return &Informer{
		store:  s,
		kind:   kind,
		logger: logger.With(zap.String("kind", kind)),
		cache:  make(map[string][]byte),
	}
}

// AddEventHandler registers an event handler that will be notified of changes.
func (i *Informer) AddEventHandler(h EventHandler) {
	i.handlersMu.Lock()
	defer i.handlersMu.Unlock()
	i.handlers = append(i.handlers, h)
}

func (i *Informer) eventHandlers() []EventHandler {
	i.handlersMu.RLock()
	defer i.handlersMu.RUnlock()
	return append([]EventHandler(nil), i.handlers...)
}

// HasSynced returns true after the initial list has completed.
func (i *Informer) HasSynced() bool {
	return i.synced.Load()
}

// GetFromCache returns the cached object for the given key, or nil if not found.
func (i *Informer) GetFromCache(key string) []byte {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.cache[key]
}

// Run performs an initial list to populate the cache, then watches for changes.
// It blocks until ctx is canceled.
func (i *Informer) Run(ctx context.Context) error {
	// Open the watch before listing so events created during the initial list are buffered.
	var ch <-chan store.WatchEvent
	for {
		var err error
		ch, err = i.store.Watch(ctx, i.kind, store.ListOptions{})
		if err == nil {
			break
		}
		i.logger.Warn("watch error, will reconnect", zap.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Phase 1: Initial list to populate the cache and enqueue all existing resources.
	if err := i.list(ctx); err != nil {
		return err
	}
	i.synced.Store(true)
	i.logger.Info("informer initial sync complete", zap.Int("cached", len(i.cache)))

	// Phase 2: Watch for ongoing changes.
	for {
		if err := i.consumeWatch(ctx, ch); err != nil {
			if ctx.Err() != nil {
				return nil // context canceled, normal shutdown
			}
			i.logger.Warn("watch error, will reconnect", zap.Error(err))
			if errors.Is(err, errWatchResyncRequired) {
				if err := i.list(ctx); err != nil {
					return fmt.Errorf("relist after watch overflow: %w", err)
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		for {
			var err error
			ch, err = i.store.Watch(ctx, i.kind, store.ListOptions{})
			if err == nil {
				break
			}
			i.logger.Warn("watch error, will reconnect", zap.Error(err))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// list performs the initial full list and populates the cache.
func (i *Informer) list(ctx context.Context) error {
	items, err := i.store.List(ctx, i.kind, store.ListOptions{})
	if err != nil {
		return err
	}

	next := make(map[string][]byte, len(items))
	for _, raw := range items {
		var meta resourceMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			i.logger.Warn("failed to parse resource metadata during list", zap.Error(err))
			continue
		}
		next[KeyFunc(meta.Metadata.Namespace, meta.Metadata.Name)] = raw
	}

	i.mu.Lock()
	previous := i.cache
	i.cache = next
	i.mu.Unlock()
	handlers := i.eventHandlers()

	for key := range previous {
		if _, exists := next[key]; exists {
			continue
		}
		for _, h := range handlers {
			h.OnDelete(key)
		}
	}
	for key := range next {
		_, existed := previous[key]
		for _, h := range handlers {
			if existed {
				h.OnUpdate(key)
			} else {
				h.OnAdd(key)
			}
		}
	}
	return nil
}

func (i *Informer) consumeWatch(ctx context.Context, ch <-chan store.WatchEvent) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil // channel closed, trigger reconnect
			}
			if ev.Type == store.EventResyncRequired {
				return errWatchResyncRequired
			}
			i.handleEvent(ev)
		}
	}
}

// handleEvent processes a single watch event — updates the cache and notifies handlers.
func (i *Informer) handleEvent(ev store.WatchEvent) {
	key := KeyFunc(ev.Namespace, ev.Name)
	handlers := i.eventHandlers()

	i.mu.Lock()
	switch ev.Type {
	case store.EventCreated:
		i.cache[key] = ev.Object
		i.mu.Unlock()
		for _, h := range handlers {
			h.OnAdd(key)
		}
	case store.EventUpdated:
		i.cache[key] = ev.Object
		i.mu.Unlock()
		for _, h := range handlers {
			h.OnUpdate(key)
		}
	case store.EventDeleted:
		delete(i.cache, key)
		i.mu.Unlock()
		for _, h := range handlers {
			h.OnDelete(key)
		}
	default:
		i.mu.Unlock()
		i.logger.Warn("unknown event type", zap.String("type", string(ev.Type)))
	}
}
