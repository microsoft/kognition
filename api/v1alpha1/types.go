// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

const (
	// APIGroup is the API group for all kognition resources.
	APIGroup = "kognition.dev"

	// Version is the current API version.
	Version = "v1alpha1"

	// APIVersion is the fully qualified API version string.
	APIVersion = APIGroup + "/" + Version
)

// TypeMeta describes the API version and kind of a resource.
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// ObjectMeta is metadata attached to every resource.
type ObjectMeta struct {
	Name              string            `json:"name" yaml:"name"`
	Namespace         string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty" yaml:"uid,omitempty"`
	ResourceVersion   int64             `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty" yaml:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp,omitempty" yaml:"creationTimestamp,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty" yaml:"ownerReferences,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty" yaml:"finalizers,omitempty"`
}

// OwnerReference identifies an owning resource.
type OwnerReference struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Name       string `json:"name" yaml:"name"`
	UID        string `json:"uid" yaml:"uid"`
	Controller bool   `json:"controller,omitempty" yaml:"controller,omitempty"`
}

// Condition represents a resource condition.
type Condition struct {
	Type               string    `json:"type" yaml:"type"`
	Status             string    `json:"status" yaml:"status"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty" yaml:"lastTransitionTime,omitempty"`
	Reason             string    `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message            string    `json:"message,omitempty" yaml:"message,omitempty"`
}

// LocalObjectReference references a resource by name within the same namespace.
type LocalObjectReference struct {
	Name string `json:"name" yaml:"name"`
}

// Phase represents the lifecycle phase of a resource.
type Phase string

const (
	PhasePending    Phase = "Pending"
	PhaseRunning    Phase = "Running"
	PhaseReady      Phase = "Ready"
	PhaseFailed     Phase = "Failed"
	PhaseDeleting   Phase = "Deleting"
	PhaseRegistered Phase = "Registered"
	PhaseValidating Phase = "Validating"
	PhaseDegraded   Phase = "Degraded"
	PhaseTerminated Phase = "Terminated"
	PhaseSkipped    Phase = "Skipped"
	PhaseTimedOut   Phase = "TimedOut"
)

// Object is the interface that all kognition resource types implement.
type Object interface {
	GetTypeMeta() TypeMeta
	GetObjectMeta() *ObjectMeta
}

// ResourceList is a generic list envelope for API responses.
type ResourceList[T Object] struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Items    []T `json:"items" yaml:"items"`
}

// Duration wraps time.Duration with JSON/YAML string marshaling (e.g. "720h").
type Duration struct {
	time.Duration
}

// MarshalJSON encodes the duration as a JSON string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.Duration.String() + `"`), nil
}

// UnmarshalJSON decodes a JSON string duration.
func (d *Duration) UnmarshalJSON(b []byte) error {
	// Strip quotes.
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// MarshalYAML encodes the duration as a YAML string.
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// UnmarshalYAML decodes a YAML string duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}
