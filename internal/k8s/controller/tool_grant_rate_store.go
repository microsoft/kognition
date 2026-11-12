// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	toolGrantRateDataAnnotation = "kognition.dev/tool-grant-rate"
	toolGrantRateLeasePrefix    = "kognition-tool-grant-rate-"
	toolGrantRateMaxRetries     = 8
)

type toolGrantRateLedger struct {
	MinuteCount int64     `json:"minuteCount"`
	HourCount   int64     `json:"hourCount"`
	DayCount    int64     `json:"dayCount"`
	MinuteReset time.Time `json:"minuteReset"`
	HourReset   time.Time `json:"hourReset"`
	DayReset    time.Time `json:"dayReset"`
}

func (s *StoreAdapter) ReserveToolGrantInvocation(ctx context.Context, reservation store.ToolGrantRateReservation) error {
	if reservation.Key == "" || reservation.Namespace == "" || reservation.Now.IsZero() {
		return errors.New("ToolGrant rate reservation key, namespace, and time are required")
	}
	if reservation.CallsPerMinute < 0 || reservation.CallsPerHour < 0 || reservation.CallsPerDay < 0 {
		return errors.New("ToolGrant rate limits must be non-negative")
	}
	namespace := s.controlNamespace
	if namespace == "" {
		namespace = reservation.Namespace
	}
	key := client.ObjectKey{Namespace: namespace, Name: toolGrantRateLeaseName(reservation.Key)}
	for attempt := 0; attempt < toolGrantRateMaxRetries; attempt++ {
		lease := &coordinationv1.Lease{}
		err := s.client.Get(ctx, key, lease)
		creating := apierrors.IsNotFound(err)
		if err != nil && !creating {
			return fmt.Errorf("get ToolGrant rate Lease: %w", err)
		}
		ledger := toolGrantRateLedger{}
		if creating {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}}
		} else if encoded := lease.Annotations[toolGrantRateDataAnnotation]; encoded != "" {
			if err := json.Unmarshal([]byte(encoded), &ledger); err != nil {
				return fmt.Errorf("decode ToolGrant rate Lease %s/%s: %w", key.Namespace, key.Name, err)
			}
		}
		if err := reserveToolGrantRate(&ledger, reservation); err != nil {
			return err
		}
		encoded, err := json.Marshal(ledger)
		if err != nil {
			return fmt.Errorf("encode ToolGrant rate Lease: %w", err)
		}
		if lease.Annotations == nil {
			lease.Annotations = make(map[string]string)
		}
		lease.Annotations[toolGrantRateDataAnnotation] = string(encoded)
		if creating {
			err = s.client.Create(ctx, lease)
		} else {
			err = s.client.Update(ctx, lease)
		}
		if err == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("persist ToolGrant rate Lease: %w", err)
		}
	}
	return fmt.Errorf("persist ToolGrant rate Lease after %d conflicts: %w", toolGrantRateMaxRetries, store.ErrConflict)
}

func toolGrantRateLeaseName(key string) string {
	digest := sha256.Sum256([]byte(key))
	return toolGrantRateLeasePrefix + hex.EncodeToString(digest[:16])
}

func reserveToolGrantRate(ledger *toolGrantRateLedger, reservation store.ToolGrantRateReservation) error {
	now := reservation.Now.UTC()
	if ledger.MinuteReset.IsZero() || !now.Before(ledger.MinuteReset) {
		ledger.MinuteCount = 0
		ledger.MinuteReset = now.Add(time.Minute)
	}
	if ledger.HourReset.IsZero() || !now.Before(ledger.HourReset) {
		ledger.HourCount = 0
		ledger.HourReset = now.Add(time.Hour)
	}
	if ledger.DayReset.IsZero() || !now.Before(ledger.DayReset) {
		ledger.DayCount = 0
		ledger.DayReset = now.Add(24 * time.Hour)
	}
	if reservation.CallsPerMinute > 0 && ledger.MinuteCount >= reservation.CallsPerMinute {
		return store.ErrQuotaExceeded
	}
	if reservation.CallsPerHour > 0 && ledger.HourCount >= reservation.CallsPerHour {
		return store.ErrQuotaExceeded
	}
	if reservation.CallsPerDay > 0 && ledger.DayCount >= reservation.CallsPerDay {
		return store.ErrQuotaExceeded
	}
	ledger.MinuteCount++
	ledger.HourCount++
	ledger.DayCount++
	return nil
}
