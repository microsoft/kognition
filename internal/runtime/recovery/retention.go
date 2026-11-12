// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package recovery

import (
	"context"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
)

type Retention struct {
	Storage  store.RuntimeRecoveryStore
	Interval time.Duration
}

func (retention *Retention) Start(ctx context.Context) error {
	interval := retention.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		bounded, stop := context.WithTimeout(ctx, 20*time.Second)
		_, err := retention.Storage.ExpireRuntimeRecovery(bounded, time.Now().UTC(), 100)
		stop()
		if err != nil && ctx.Err() == nil {
			return ErrUnavailable
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (*Retention) NeedLeaderElection() bool { return true }
