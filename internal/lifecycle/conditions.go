// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

import "time"

// ConditionType defines well-known condition types.
type ConditionType string

const (
	// ConditionReady indicates the resource is fully operational.
	ConditionReady ConditionType = "Ready"

	// ConditionProgressing indicates the resource is making progress toward Ready.
	ConditionProgressing ConditionType = "Progressing"

	// ConditionHealthy indicates the resource is healthy (runtime health check).
	ConditionHealthy ConditionType = "Healthy"

	// ConditionBudgetExceeded indicates the resource has exceeded its token/cost budget.
	ConditionBudgetExceeded ConditionType = "BudgetExceeded"

	// ConditionPolicyViolation indicates an active policy violation on the resource.
	ConditionPolicyViolation ConditionType = "PolicyViolation"

	// ConditionDriftDetected indicates configuration drift from the declared spec.
	ConditionDriftDetected ConditionType = "DriftDetected"

	// ConditionProbeHealthy indicates the agent health probe is passing.
	ConditionProbeHealthy ConditionType = "ProbeHealthy"

	// ConditionProbeReady indicates the agent readiness probe is passing.
	ConditionProbeReady ConditionType = "ProbeReady"

	// ConditionReasoningHealthy indicates the agent reasoning probe is passing.
	ConditionReasoningHealthy ConditionType = "ReasoningHealthy"

	// ConditionQuarantined indicates the agent instance has been quarantined.
	ConditionQuarantined ConditionType = "Quarantined"
)

// ConditionStatus represents the status of a condition.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition represents a resource condition (mirrors the API type for manipulation).
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// SetCondition sets or updates a condition in the slice. If the condition type
// already exists and the status changes, LastTransitionTime is updated.
// Returns the new condition slice.
func SetCondition(conditions []Condition, condType ConditionType, status ConditionStatus, reason, message string) []Condition {
	now := time.Now().UTC().Format(time.RFC3339)
	newCond := Condition{
		Type:               string(condType),
		Status:             string(status),
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}

	for i, c := range conditions {
		if c.Type == string(condType) {
			// Preserve transition time if status hasn't changed.
			if c.Status == string(status) {
				newCond.LastTransitionTime = c.LastTransitionTime
			}
			conditions[i] = newCond
			return conditions
		}
	}

	return append(conditions, newCond)
}

// GetCondition returns the condition with the given type, or nil if not found.
func GetCondition(conditions []Condition, condType ConditionType) *Condition {
	for i := range conditions {
		if conditions[i].Type == string(condType) {
			return &conditions[i]
		}
	}
	return nil
}

// RemoveCondition removes a condition by type. Returns the updated slice.
func RemoveCondition(conditions []Condition, condType ConditionType) []Condition {
	result := make([]Condition, 0, len(conditions))
	for _, c := range conditions {
		if c.Type != string(condType) {
			result = append(result, c)
		}
	}
	return result
}

// IsConditionTrue returns true if the named condition exists and has status "True".
func IsConditionTrue(conditions []Condition, condType ConditionType) bool {
	c := GetCondition(conditions, condType)
	return c != nil && c.Status == string(ConditionTrue)
}

// IsConditionFalse returns true if the named condition exists and has status "False".
func IsConditionFalse(conditions []Condition, condType ConditionType) bool {
	c := GetCondition(conditions, condType)
	return c != nil && c.Status == string(ConditionFalse)
}
