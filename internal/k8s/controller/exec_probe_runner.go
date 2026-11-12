// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

const (
	execProbeLabel        = "kognition.dev/exec-probe"
	execProbeCleanupTTL   = int32(60)
	execProbePollInterval = 250 * time.Millisecond
)

// KubernetesExecProbeRunner runs an exec health probe in a controller-created
// one-shot Job. The Job has no service-account token and a deny-all egress
// NetworkPolicy, so Tool argv never execute in the controller process.
type KubernetesExecProbeRunner struct {
	client client.Client
	poll   time.Duration
	wait   func(context.Context, *batchv1.Job) error
}

// NewKubernetesExecProbeRunner creates the production Kubernetes exec probe runner.
func NewKubernetesExecProbeRunner(c client.Client) *KubernetesExecProbeRunner {
	return &KubernetesExecProbeRunner{client: c, poll: execProbePollInterval}
}

// RunExecProbe creates an isolated Job for one direct command invocation.
func (r *KubernetesExecProbeRunner) RunExecProbe(ctx context.Context, tool *v1alpha1.Tool, target intctrl.ExecProbeTarget, command []string) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("Kubernetes exec probe runner is not configured")
	}
	if tool == nil || strings.TrimSpace(tool.ObjectMeta.Namespace) == "" {
		return fmt.Errorf("exec probe Tool namespace is required")
	}
	if strings.TrimSpace(target.Image) == "" || len(command) == 0 {
		return fmt.Errorf("exec probe target image and command are required")
	}

	probeID := uuid.NewString()
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kognition",
		execProbeLabel:                 probeID,
		"kognition.dev/tool":           tool.ObjectMeta.Name,
		"kognition.dev/managed":        "exec-probe",
	}
	networkPolicy := newExecProbeNetworkPolicy(tool.ObjectMeta.Namespace, probeID)
	if err := r.client.Create(ctx, networkPolicy); err != nil {
		return fmt.Errorf("create exec probe NetworkPolicy: %w", err)
	}
	defer func() { _ = r.client.Delete(context.Background(), networkPolicy) }()

	job := newExecProbeJob(tool.ObjectMeta.Namespace, labels, target, command)
	if err := r.client.Create(ctx, job); err != nil {
		return fmt.Errorf("create exec probe Job: %w", err)
	}
	defer func() {
		_ = r.client.Delete(context.Background(), job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()

	if r.wait != nil {
		if err := r.wait(ctx, job); err != nil {
			return err
		}
		return nil
	}
	return r.waitForJob(ctx, job)
}

func newExecProbeNetworkPolicy(namespace, probeID string) *networkingv1.NetworkPolicy {
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", execProbeLabel: probeID}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "kognition-exec-probe-", Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		},
	}
}

func newExecProbeJob(namespace string, labels map[string]string, target intctrl.ExecProbeTarget, command []string) *batchv1.Job {
	backoffLimit := int32(0)
	cleanupTTL := int32(execProbeCleanupTTL)
	activeDeadlineSeconds := int64(math.Max(1, math.Ceil(target.Timeout.Seconds())))
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	restartPolicy := corev1.RestartPolicyNever
	automountServiceAccountToken := false
	resources := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	if target.CPUMilli > 0 {
		cpu := resource.NewMilliQuantity(target.CPUMilli, resource.DecimalSI)
		resources.Limits[corev1.ResourceCPU] = *cpu
		resources.Requests[corev1.ResourceCPU] = *cpu
	}
	if target.MemoryMB > 0 {
		memory := resource.NewQuantity(target.MemoryMB*1024*1024, resource.BinarySI)
		resources.Limits[corev1.ResourceMemory] = *memory
		resources.Requests[corev1.ResourceMemory] = *memory
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "kognition-exec-probe-", Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &cleanupTTL,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                restartPolicy,
					AutomountServiceAccountToken: &automountServiceAccountToken,
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &runAsUser, RunAsGroup: &runAsGroup, RunAsNonRoot: &runAsNonRoot, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:            "exec-probe",
						Image:           target.Image,
						Command:         append([]string(nil), command...),
						Resources:       resources,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation, ReadOnlyRootFilesystem: &readOnlyRootFilesystem, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
				},
			},
		},
	}
}

func (r *KubernetesExecProbeRunner) waitForJob(ctx context.Context, job *batchv1.Job) error {
	poll := r.poll
	if poll <= 0 {
		poll = execProbePollInterval
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		var current batchv1.Job
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &current); err != nil {
			return fmt.Errorf("read exec probe Job: %w", err)
		}
		if !matchesExecProbeJob(&current, job.Labels) {
			return fmt.Errorf("read exec probe Job: sandbox labels do not match the created Job")
		}
		if current.Status.Succeeded > 0 {
			return nil
		}
		if current.Status.Failed > 0 {
			return fmt.Errorf("sandbox Job failed")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox Job did not complete: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func matchesExecProbeJob(job *batchv1.Job, expectedLabels map[string]string) bool {
	if job == nil {
		return false
	}
	for key, value := range expectedLabels {
		if job.Labels[key] != value {
			return false
		}
	}
	return true
}
