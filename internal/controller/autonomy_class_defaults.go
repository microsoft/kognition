// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const autonomyClassKind = "AutonomyClass"

// DefaultAutonomyClasses returns the built-in autonomy class resources.
func DefaultAutonomyClasses() []v1alpha1.AutonomyClass {
	return []v1alpha1.AutonomyClass{
		{
			TypeMeta: v1alpha1.TypeMeta{
				Kind:       autonomyClassKind,
				APIVersion: "v1alpha1",
			},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "supervised",
				Namespace: "system",
			},
			Spec: v1alpha1.AutonomyClassSpec{
				Level:            10,
				RequiresApproval: true,
				ApprovalPolicy: &v1alpha1.ApprovalPolicy{
					ToolInvocation:        "always",
					MemoryWrite:           "always",
					ExternalCommunication: "always",
					CostThreshold:         0,
				},
				Constraints: &v1alpha1.AutonomyConstraints{
					MaxConcurrentActions:    1,
					MaxTokenBudgetPerAction: 1000,
				},
			},
		},
		{
			TypeMeta: v1alpha1.TypeMeta{
				Kind:       autonomyClassKind,
				APIVersion: "v1alpha1",
			},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "assisted",
				Namespace: "system",
			},
			Spec: v1alpha1.AutonomyClassSpec{
				Level:            50,
				RequiresApproval: false,
				ApprovalPolicy: &v1alpha1.ApprovalPolicy{
					ToolInvocation:        "high-risk-only",
					MemoryWrite:           "high-risk-only",
					ExternalCommunication: "always",
					CostThreshold:         5000,
				},
				Constraints: &v1alpha1.AutonomyConstraints{
					MaxConcurrentActions:    5,
					MaxTokenBudgetPerAction: 10000,
				},
			},
		},
		{
			TypeMeta: v1alpha1.TypeMeta{
				Kind:       autonomyClassKind,
				APIVersion: "v1alpha1",
			},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "autonomous",
				Namespace: "system",
			},
			Spec: v1alpha1.AutonomyClassSpec{
				Level:            90,
				RequiresApproval: false,
				ApprovalPolicy: &v1alpha1.ApprovalPolicy{
					ToolInvocation:        "never",
					MemoryWrite:           "never",
					ExternalCommunication: "high-risk-only",
					CostThreshold:         50000,
				},
				Constraints: &v1alpha1.AutonomyConstraints{
					MaxConcurrentActions:    20,
					MaxTokenBudgetPerAction: 100000,
				},
			},
		},
	}
}

// EnsureDefaultAutonomyClasses ensures the default AutonomyClass resources exist in the store.
func EnsureDefaultAutonomyClasses(ctx context.Context, s store.Store, logger *zap.Logger) error {
	for _, ac := range DefaultAutonomyClasses() {
		data, err := json.Marshal(ac)
		if err != nil {
			return fmt.Errorf("marshal AutonomyClass %s: %w", ac.Name, err)
		}

		err = s.Create(ctx, autonomyClassKind, ac.Namespace, ac.Name, data)
		if err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				logger.Debug("default AutonomyClass already exists", zap.String("name", ac.Name))
				continue
			}
			return fmt.Errorf("create default AutonomyClass %s: %w", ac.Name, err)
		}
		logger.Info("created default AutonomyClass", zap.String("name", ac.Name), zap.Int("level", ac.Spec.Level))
	}
	return nil
}
