// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/ingestion"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

const (
	ingestInputKey          = "input.json"
	ingestInputPath         = "/var/run/kognition-ingest/input.json"
	ingestInputMaxSize      = 900 * 1024
	providerConfigKey       = "index-config.json"
	providerConfigPath      = "/var/run/kognition-index-config/index-config.json"
	providerTLSCertKey      = "client.crt"
	providerTLSKeyKey       = "client.key"
	providerTLSCAKey        = "ca.crt"
	providerTLSPath         = "/var/run/kognition-index-config"
	providerSpoolPath       = "/var/run/kognition/index/chunks.jsonl"
	providerAnnotation      = "kognition.dev/knowledge-provider"
	providerInputAnnotation = "kognition.dev/knowledge-provider-input"
)

type providerTransportConfig struct {
	ProviderName          string                   `json:"providerName"`
	ProviderAddress       string                   `json:"providerAddress"`
	OperationID           string                   `json:"operationId"`
	Dimension             int                      `json:"dimension"`
	ClassUID              string                   `json:"classUID"`
	ClassGeneration       int64                    `json:"classGeneration"`
	Scope                 *protocol.KnowledgeScope `json:"scope"`
	TLS                   providerTLSConfig        `json:"tls"`
	EmbeddingModelBinding *api.ModelVersionBinding `json:"embeddingModelBinding,omitempty"`
}

type providerTLSConfig struct {
	CertificateFile string `json:"certificateFile"`
	PrivateKeyFile  string `json:"privateKeyFile"`
	TrustBundleFile string `json:"trustBundleFile"`
	ProviderPeerURI string `json:"providerPeerUri"`
	ServerName      string `json:"serverName"`
}

type ingestArtifacts struct {
	payload      []byte
	extraEnv     []corev1.EnvVar
	providerData map[string][]byte
}

type providerConfirmation struct {
	OperationID        string `json:"operationId"`
	DocumentCount      int64  `json:"documentCount"`
	IndexedChunks      int64  `json:"indexedChunks"`
	SourceDigest       string `json:"sourceDigest"`
	BackendOperationID string `json:"backendOperationId"`
}

type ingestSummary struct {
	Documents            int64                        `json:"documents"`
	Chunks               int64                        `json:"chunks"`
	SourceDigest         string                       `json:"sourceDigest"`
	BackendBinding       *api.KnowledgeBackendBinding `json:"backendBinding"`
	ProviderConfirmation *providerConfirmation        `json:"providerConfirmation"`
}

// KBIngesterLauncher implements intctrl.IngesterLauncher by creating a
// Kubernetes Job that runs the `kognition-kb-ingester` image. The Job is named
// deterministically per (KB, generation) so repeated Ensure calls are
// idempotent.
type KBIngesterLauncher struct {
	client          client.Client
	store           store.Store
	logger          *zap.Logger
	image           string
	imagePullPol    corev1.PullPolicy
	serviceAccount  string
	namespace       string // controller namespace (used when SecretRef is not in KB namespace)
	provider        *KBIngesterProviderOptions
	bindingResolver *knowledge.BindingResolver
	trustedReader   client.Reader
}

// NewKBIngesterLauncher constructs a launcher. When image is empty the
// launcher is a no-op — the reconciler falls back to its simulated behaviour.
// controllerNamespace is used as a fallback lookup namespace when a referenced
// SecretRef cannot be found in the KB's own namespace.
func NewKBIngesterLauncher(c client.Client, s store.Store, logger *zap.Logger,
	image, serviceAccount, controllerNamespace string) *KBIngesterLauncher {
	return &KBIngesterLauncher{
		client:         c,
		store:          s,
		logger:         logger.With(zap.String("component", "kb-ingester-launcher")),
		image:          image,
		imagePullPol:   corev1.PullIfNotPresent,
		serviceAccount: serviceAccount,
		namespace:      controllerNamespace,
	}
}

// WithProvider attaches installation-owned transport settings and the
// uncached resolver used to bind tenant classes to immutable provider state.
func (l *KBIngesterLauncher) WithProvider(provider KBIngesterProviderOptions, resolver *knowledge.BindingResolver, reader client.Reader) *KBIngesterLauncher {
	l.provider = &provider
	l.bindingResolver = resolver
	l.trustedReader = reader
	return l
}

// Ensure implements intctrl.IngesterLauncher.
func (l *KBIngesterLauncher) Ensure(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass) (intctrl.IngestState, intctrl.IngestStats, error) {
	if l.image == "" {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, nil
	}
	if kb.ObjectMeta.Namespace != "" && kb.ObjectMeta.Namespace == l.namespace {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("ingestion workloads cannot use installation namespace %q; create the KnowledgeBase in a separate enrolled workload namespace", l.namespace)
	}

	// The Job name is scoped to the KB's *next* generation so a Refresh can
	// launch a fresh Job while the previous generation's status is still
	// visible on the KB.
	targetGen := kb.Status.Generation + 1
	jobName := ingestJobName(kb.ObjectMeta.Name, targetGen)

	// Look up existing Job first.
	existing := &batchv1.Job{}
	err := l.client.Get(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName}, existing)
	switch {
	case err == nil:
		if !hasControllerOwner(existing.OwnerReferences, "KnowledgeBase", kb.ObjectMeta.Name, kb.ObjectMeta.UID) {
			return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("existing ingest job %s/%s is not controlled by KnowledgeBase %s", kb.ObjectMeta.Namespace, jobName, kb.ObjectMeta.Name)
		}
		state, stats, interpretErr := l.interpretJob(ctx, existing)
		if interpretErr != nil || state != intctrl.IngestRunning {
			return state, stats, interpretErr
		}
		if err := l.ensureRunningJobInput(ctx, kb, kbc, jobName, kbOwnerRef(kb)); err != nil {
			return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("ensure ingest input secret: %w", err)
		}
		return state, stats, nil
	case apierrors.IsNotFound(err):
		// fall through to build+create
	default:
		return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("get ingest job: %w", err)
	}

	// Build the payload and the Job spec.
	artifacts, err := l.buildArtifacts(ctx, kb, kbc, jobName)
	if err != nil {
		return intctrl.IngestFailed, intctrl.IngestStats{Error: err.Error()}, nil
	}
	if len(artifacts.payload) > ingestInputMaxSize {
		return intctrl.IngestFailed, intctrl.IngestStats{Error: fmt.Sprintf("ingest input exceeds %d bytes", ingestInputMaxSize)}, nil
	}

	secret := l.buildInputSecretWithProviderData(kb, jobName, artifacts.payload, artifacts.extraEnv, artifacts.providerData)
	if err := l.ensureInputSecret(ctx, secret); err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("create ingest input secret: %w", err)
	}
	job := l.buildJob(kb, jobName, secret.Name, artifacts.extraEnv)
	if artifacts.providerData != nil {
		configureProviderJob(job, secret.Name, l.provider.Name)
	}
	if err := l.client.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := l.client.Get(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName}, existing); err != nil {
				return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("get raced ingest job: %w", err)
			}
			if !hasControllerOwner(existing.OwnerReferences, "KnowledgeBase", kb.ObjectMeta.Name, kb.ObjectMeta.UID) {
				return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("existing ingest job %s/%s is not controlled by KnowledgeBase %s", kb.ObjectMeta.Namespace, jobName, kb.ObjectMeta.Name)
			}
			return l.interpretJob(ctx, existing)
		}
		return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("create ingest job: %w", err)
	}
	l.logger.Info("launched kb ingest job",
		zap.String("job", jobName),
		zap.String("kb", kb.ObjectMeta.Name),
		zap.String("namespace", kb.ObjectMeta.Namespace),
		zap.Int64("generation", targetGen),
	)
	return intctrl.IngestRunning, intctrl.IngestStats{}, nil
}

func (l *KBIngesterLauncher) ensureInputSecret(ctx context.Context, desired *corev1.Secret) error {
	if err := l.client.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}

	existing := &corev1.Secret{}
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := l.client.Get(ctx, key, existing); err != nil {
		return fmt.Errorf("get existing secret: %w", err)
	}
	if existing.Type != desired.Type ||
		!reflect.DeepEqual(existing.Data, desired.Data) ||
		!reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return fmt.Errorf("existing secret %s/%s does not match the expected ingestion input", desired.Namespace, desired.Name)
	}
	return nil
}

func (l *KBIngesterLauncher) ensureRunningJobInput(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass, jobName string, owner metav1.OwnerReference) error {
	key := types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName + "-input"}
	existing := &corev1.Secret{}
	if err := l.client.Get(ctx, key, existing); err == nil {
		if !hasControllerOwner(existing.OwnerReferences, owner.Kind, owner.Name, string(owner.UID)) {
			return fmt.Errorf("existing secret %s/%s is not controlled by %s %s", key.Namespace, key.Name, owner.Kind, owner.Name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if kbc.Spec.BackendRef != nil {
		return fmt.Errorf("provider Job input %s/%s is unavailable and cannot be reconstructed", key.Namespace, key.Name)
	}

	artifacts, err := l.buildArtifacts(ctx, kb, kbc, jobName)
	if err != nil {
		return err
	}
	if len(artifacts.payload) > ingestInputMaxSize {
		return fmt.Errorf("ingest input exceeds %d bytes", ingestInputMaxSize)
	}
	desired := l.buildInputSecretWithProviderData(kb, jobName, artifacts.payload, artifacts.extraEnv, artifacts.providerData)
	desired.OwnerReferences = []metav1.OwnerReference{owner}
	return l.ensureInputSecret(ctx, desired)
}

// Reset implements intctrl.IngesterLauncher. It deletes the (terminal) ingest
// Job for the KB's pending generation so a subsequent Ensure creates a fresh
// one. The reconciler calls this to retry a Failed KB. Idempotent: a no-op
// when no launcher image is configured or the Job no longer exists.
func (l *KBIngesterLauncher) Reset(ctx context.Context, kb *api.KnowledgeBase) error {
	if l.image == "" {
		return nil
	}
	targetGen := kb.Status.Generation + 1
	jobName := ingestJobName(kb.ObjectMeta.Name, targetGen)
	// Background propagation so the Job's pods are cleaned up too.
	policy := metav1.DeletePropagationBackground
	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName}
	jobExists := false
	if err := l.client.Get(ctx, jobKey, job); err == nil {
		if !hasControllerOwner(job.OwnerReferences, "KnowledgeBase", kb.ObjectMeta.Name, kb.ObjectMeta.UID) {
			return fmt.Errorf("existing ingest job %s/%s is not controlled by KnowledgeBase %s", kb.ObjectMeta.Namespace, jobName, kb.ObjectMeta.Name)
		}
		jobExists = true
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get ingest job for reset: %w", err)
	}
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName + "-input"}
	secretExists := false
	if err := l.client.Get(ctx, secretKey, secret); err == nil {
		if !hasControllerOwner(secret.OwnerReferences, "KnowledgeBase", kb.ObjectMeta.Name, kb.ObjectMeta.UID) {
			return fmt.Errorf("existing ingest input secret %s/%s is not controlled by KnowledgeBase %s", kb.ObjectMeta.Namespace, secretKey.Name, kb.ObjectMeta.Name)
		}
		secretExists = true
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get ingest input secret for reset: %w", err)
	}
	if jobExists {
		if err := l.client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}, client.Preconditions{UID: &job.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ingest job for reset: %w", err)
		}
	}
	if secretExists {
		if err := l.client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ingest input secret for reset: %w", err)
		}
	}
	l.logger.Info("reset failed kb ingest job for retry",
		zap.String("job", jobName),
		zap.String("kb", kb.ObjectMeta.Name))
	return nil
}

// interpretJob maps Job status → IngestState + stats. When the Job succeeded
// it also reads the pod's terminationMessage to recover the ingester summary.
func (l *KBIngesterLauncher) interpretJob(ctx context.Context, job *batchv1.Job) (intctrl.IngestState, intctrl.IngestStats, error) {
	if err := ctx.Err(); err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, err
	}
	// Terminal condition check first.
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			return intctrl.IngestFailed, intctrl.IngestStats{Error: fmt.Sprintf("%s: %s", c.Reason, c.Message)}, nil
		case batchv1.JobComplete:
			return l.interpretSuccessfulJob(ctx, job)
		}
	}
	if job.Status.Succeeded > 0 {
		return l.interpretSuccessfulJob(ctx, job)
	}
	return intctrl.IngestRunning, intctrl.IngestStats{}, nil
}

func (l *KBIngesterLauncher) interpretSuccessfulJob(ctx context.Context, job *batchv1.Job) (intctrl.IngestState, intctrl.IngestStats, error) {
	stats := intctrl.IngestStats{}
	summary := l.readSummary(ctx, job, &stats)
	if err := ctx.Err(); err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, err
	}
	if job.Annotations[providerAnnotation] == "" {
		return intctrl.IngestSucceeded, stats, nil
	}
	if summary == nil {
		stats.Error = "provider ingest completed without a valid confirmation"
		return intctrl.IngestFailed, stats, nil
	}
	if err := l.verifyProviderSummary(ctx, job, summary); err != nil {
		stats.Error = err.Error()
		return intctrl.IngestFailed, stats, nil
	}
	return intctrl.IngestSucceeded, stats, nil
}

// readSummary tails the completed pod's containerStatuses to extract the
// JSON summary the ingester wrote to /dev/termination-log.
func (l *KBIngesterLauncher) readSummary(ctx context.Context, job *batchv1.Job, stats *intctrl.IngestStats) *ingestSummary {
	pods := &corev1.PodList{}
	if err := l.client.List(ctx, pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		l.logger.Warn("list pods for ingest job", zap.String("job", job.Name), zap.Error(err))
		return nil
	}
	for _, pod := range pods.Items {
		if job.UID == "" || !hasControllerOwner(pod.OwnerReferences, "Job", job.Name, string(job.UID)) {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "ingester" || cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
				continue
			}
			msg := cs.State.Terminated.Message
			if msg == "" {
				continue
			}
			var s ingestSummary
			if err := json.Unmarshal([]byte(msg), &s); err != nil {
				continue
			}
			stats.Documents = s.Documents
			stats.Chunks = s.Chunks
			stats.SourceDigest = s.SourceDigest
			stats.BackendBinding = s.BackendBinding
			return &s
		}
	}
	return nil
}

func (l *KBIngesterLauncher) verifyProviderSummary(ctx context.Context, job *batchv1.Job, summary *ingestSummary) error {
	if l.provider == nil || l.bindingResolver == nil {
		return fmt.Errorf("provider completion cannot be verified")
	}
	if job.Annotations[providerAnnotation] != l.provider.Name {
		return fmt.Errorf("provider confirmation does not match the registered provider")
	}
	secretName := job.Annotations[providerInputAnnotation]
	if secretName == "" {
		return fmt.Errorf("provider Job has no trusted input reference")
	}
	reader := l.trustedReader
	if reader == nil {
		reader = l.client
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: secretName}, secret); err != nil {
		return fmt.Errorf("read provider Job input: %w", err)
	}
	if !sharesControllerOwner(secret.OwnerReferences, job.OwnerReferences) {
		return fmt.Errorf("provider Job input has an unexpected owner")
	}
	config, err := decodeProviderTransportConfig(secret.Data[providerConfigKey])
	if err != nil {
		return err
	}
	if config.ProviderName != l.provider.Name || config.ProviderAddress != l.provider.Address {
		return fmt.Errorf("provider Job transport differs from the registered provider")
	}
	kbName := job.Labels["kognition.dev/kb"]
	if kbName == "" {
		return fmt.Errorf("provider Job has no knowledge base identity")
	}
	pending, err := l.bindingResolver.ResolvePending(ctx, types.NamespacedName{Namespace: job.Namespace, Name: kbName})
	if err != nil {
		return fmt.Errorf("resolve provider completion binding: %w", err)
	}
	if pending.ProviderName != config.ProviderName || pending.EmbeddingDimension != config.Dimension ||
		pending.ClassUID != config.ClassUID || pending.ClassGeneration != config.ClassGeneration || !proto.Equal(pending.Scope, config.Scope) {
		return fmt.Errorf("provider completion binding is stale")
	}
	confirmation := summary.ProviderConfirmation
	binding := summary.BackendBinding
	if confirmation == nil || binding == nil || config.Scope == nil ||
		confirmation.OperationID != config.OperationID || confirmation.DocumentCount != summary.Documents ||
		confirmation.IndexedChunks != summary.Chunks || confirmation.SourceDigest != summary.SourceDigest ||
		confirmation.DocumentCount < 0 || confirmation.IndexedChunks < 0 || strings.TrimSpace(confirmation.SourceDigest) == "" ||
		strings.TrimSpace(confirmation.BackendOperationID) == "" || binding.Generation != config.Scope.Generation ||
		binding.ProviderName != config.ProviderName || binding.ClassUID != config.ClassUID ||
		binding.ClassGeneration != config.ClassGeneration || binding.ConfigurationUID != config.Scope.ConfigurationUid ||
		binding.ConfigurationDigest != config.Scope.ConfigurationDigest {
		return fmt.Errorf("provider confirmation does not match the trusted launch state")
	}
	binding.ConfigurationAPIGroup = config.Scope.ConfigurationApiGroup
	binding.ConfigurationKind = config.Scope.ConfigurationKind
	binding.ConfigurationName = config.Scope.ConfigurationName
	binding.OperationID = config.OperationID
	binding.BackendOperationID = confirmation.BackendOperationID
	if config.EmbeddingModelBinding != nil {
		if _, err := runtimeauth.ResolveModelConfigurationBinding(ctx, l.store, *config.EmbeddingModelBinding); err != nil {
			return fmt.Errorf("embedding Model configuration changed during provider indexing: %w", err)
		}
		snapshot := *config.EmbeddingModelBinding
		binding.EmbeddingModelBinding = &snapshot
	}
	return nil
}

func decodeProviderTransportConfig(content []byte) (*providerTransportConfig, error) {
	if len(content) == 0 || len(content) > ingestInputMaxSize {
		return nil, fmt.Errorf("provider Job transport configuration has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var config providerTransportConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode provider Job transport configuration: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || config.Scope == nil || config.ProviderName == "" || config.ProviderAddress == "" ||
		config.OperationID == "" || config.Dimension < 1 || config.ClassUID == "" || config.ClassGeneration < 1 {
		return nil, fmt.Errorf("provider Job transport configuration is incomplete")
	}
	return &config, nil
}

func sharesControllerOwner(left, right []metav1.OwnerReference) bool {
	for _, owner := range right {
		if owner.Controller != nil && *owner.Controller && hasControllerOwner(left, owner.Kind, owner.Name, string(owner.UID)) {
			return true
		}
	}
	return false
}

// buildJob builds the Job spec with the payload mounted from a Secret. The container
// writes its summary to /dev/termination-log so status is readable from Pod
// status without pulling logs. `extraEnv` carries provider-specific auth vars
// (e.g. AZURE_TENANT_ID/CLIENT_ID/CLIENT_SECRET for AAD-based embedding).
func (l *KBIngesterLauncher) buildJob(kb *api.KnowledgeBase, name, secretName string, extraEnv []corev1.EnvVar) *batchv1.Job {
	// Tolerate transient startup races (an ingest launched before its URL
	// source Service or the pgvector Pod is Ready). Kubernetes backs off
	// exponentially between retries, so 6 attempts spans several minutes —
	// enough for dependencies to come up without leaving the KB stuck Failed.
	backoff := int32(6)
	ttl := int32(3600)
	env := []corev1.EnvVar{{Name: "KOGNITION_INGEST_INPUT_FILE", Value: ingestInputPath}}
	if kb.ObjectMeta.Annotations["kognition.dev/ingest-allow-private-networks"] == "true" {
		env = append(env, corev1.EnvVar{Name: "KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS", Value: "true"})
	}
	for _, item := range extraEnv {
		env = append(env, corev1.EnvVar{
			Name: item.Name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  item.Name,
			}},
		})
	}
	readOnly := true
	automount := false
	runAsUser := int64(1000)
	mode := int32(0444)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kb.ObjectMeta.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kognition",
				"kognition.dev/kb":             kb.ObjectMeta.Name,
				"kognition.dev/component":      "kb-ingester",
			},
			OwnerReferences: []metav1.OwnerReference{
				kbOwnerRef(kb),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kognition",
						"kognition.dev/kb":             kb.ObjectMeta.Name,
						"kognition.dev/component":      "kb-ingester",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyOnFailure,
					ServiceAccountName:           l.serviceAccount,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &readOnly, RunAsUser: &runAsUser,
						RunAsGroup: &runAsUser, FSGroup: &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "ingester",
						Image: l.image,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &automount,
							ReadOnlyRootFilesystem:   &readOnly,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ImagePullPolicy: l.imagePullPol,
						Env:             env,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "ingest-input",
							MountPath: ingestInputPath,
							SubPath:   ingestInputKey,
							ReadOnly:  readOnly,
						}, {
							Name: "ingest-tmp", MountPath: "/tmp",
						}},
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "ingest-input",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName:  secretName,
							DefaultMode: &mode,
						}},
					}, {
						Name: "ingest-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
}

func (l *KBIngesterLauncher) buildInputSecret(kb *api.KnowledgeBase, jobName string, payload []byte, extraEnv []corev1.EnvVar) *corev1.Secret {
	return l.buildInputSecretWithProviderData(kb, jobName, payload, extraEnv, nil)
}

func (l *KBIngesterLauncher) buildInputSecretWithProviderData(kb *api.KnowledgeBase, jobName string, payload []byte, extraEnv []corev1.EnvVar, providerData map[string][]byte) *corev1.Secret {
	data := map[string][]byte{ingestInputKey: payload}
	for _, item := range extraEnv {
		data[item.Name] = []byte(item.Value)
	}
	for key, value := range providerData {
		data[key] = value
	}
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName + "-input",
			Namespace:       kb.ObjectMeta.Namespace,
			OwnerReferences: []metav1.OwnerReference{kbOwnerRef(kb)},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kognition",
				"kognition.dev/kb":             kb.ObjectMeta.Name,
				"kognition.dev/component":      "kb-ingester",
			},
		},
		Type:      corev1.SecretTypeOpaque,
		Data:      data,
		Immutable: &immutable,
	}
}

func (l *KBIngesterLauncher) buildArtifacts(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass, jobName string) (*ingestArtifacts, error) {
	if kbc.Spec.BackendRef == nil {
		payload, environment, err := l.buildPayload(ctx, kb, kbc)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(payload)
		return &ingestArtifacts{payload: encoded, extraEnv: environment}, err
	}
	if l.provider == nil || l.bindingResolver == nil || l.trustedReader == nil {
		return nil, fmt.Errorf("knowledge provider ingestion is not configured")
	}
	pending, err := l.bindingResolver.ResolvePending(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: kb.ObjectMeta.Name})
	if err != nil {
		return nil, fmt.Errorf("resolve pending knowledge binding: %w", err)
	}
	if pending.ProviderName != l.provider.Name {
		return nil, fmt.Errorf("resolved knowledge provider does not match the registered transport")
	}
	payload, environment, err := l.payloadBuilder().BuildProvider(ctx, kb, kbc, pending.EmbeddingDimension)
	if err != nil {
		return nil, err
	}
	embedding, ok := payload["embedding"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("provider embedding payload is invalid")
	}
	modelBinding, ok := embedding["modelBinding"].(*api.ModelVersionBinding)
	if !ok || modelBinding == nil {
		return nil, fmt.Errorf("provider embedding Model binding is required")
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	tlsSecret := &corev1.Secret{}
	if err := l.trustedReader.Get(ctx, types.NamespacedName{Namespace: l.namespace, Name: l.provider.TLSSecretName}, tlsSecret); err != nil {
		return nil, fmt.Errorf("read knowledge provider TLS secret: %w", err)
	}
	certificate, certificateOK := tlsSecret.Data[l.provider.TLSCertificateKey]
	privateKey, privateKeyOK := tlsSecret.Data[l.provider.TLSPrivateKeyKey]
	trustBundle, trustBundleOK := tlsSecret.Data[l.provider.TLSTrustBundleKey]
	if !certificateOK || !privateKeyOK || !trustBundleOK || len(certificate) == 0 || len(privateKey) == 0 || len(trustBundle) == 0 {
		return nil, fmt.Errorf("knowledge provider TLS secret is incomplete")
	}
	transport := providerTransportConfig{
		ProviderName: l.provider.Name, ProviderAddress: l.provider.Address,
		OperationID: kb.ObjectMeta.Namespace + "/" + jobName, Dimension: pending.EmbeddingDimension,
		ClassUID: pending.ClassUID, ClassGeneration: pending.ClassGeneration, Scope: pending.Scope,
		EmbeddingModelBinding: modelBinding,
		TLS: providerTLSConfig{
			CertificateFile: providerTLSPath + "/" + providerTLSCertKey,
			PrivateKeyFile:  providerTLSPath + "/" + providerTLSKeyKey,
			TrustBundleFile: providerTLSPath + "/" + providerTLSCAKey,
			ProviderPeerURI: l.provider.PeerURI, ServerName: l.provider.ServerName,
		},
	}
	encodedConfig, err := json.Marshal(transport)
	if err != nil {
		return nil, fmt.Errorf("encode knowledge provider transport: %w", err)
	}
	return &ingestArtifacts{
		payload: encodedPayload, extraEnv: toKubernetesEnv(environment),
		providerData: map[string][]byte{providerConfigKey: encodedConfig, providerTLSCertKey: certificate, providerTLSKeyKey: privateKey, providerTLSCAKey: trustBundle},
	}, nil
}

func configureProviderJob(job *batchv1.Job, secretName, providerName string) {
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[providerAnnotation] = providerName
	job.Annotations[providerInputAnnotation] = secretName
	container := &job.Spec.Template.Spec.Containers[0]
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "KOGNITION_KNOWLEDGE_INDEX_CONFIG_FILE", Value: providerConfigPath},
		corev1.EnvVar{Name: "KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE", Value: providerSpoolPath},
	)
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: "provider-config", MountPath: providerTLSPath, ReadOnly: true},
		corev1.VolumeMount{Name: "provider-spool", MountPath: "/var/run/kognition/index"},
	)
	mode := int32(0440)
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
		corev1.Volume{Name: "provider-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: &mode}}},
		corev1.Volume{Name: "provider-spool", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	)
}

// buildPayload assembles the JSON envelope the ingester consumes. It resolves
// pgvector DSN, blob credentials, and embedding-model credentials via the
// existing SecretRef+annotation convention used by the dispatcher. The second
// return value is a set of extra env vars that must be set on the Job pod
// (e.g. AZURE_* triple for Azure-identity embedding credentials).
func (l *KBIngesterLauncher) buildPayload(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass) (map[string]any, []corev1.EnvVar, error) {
	payload, environment, err := l.payloadBuilder().Build(ctx, kb, kbc)
	return payload, toKubernetesEnv(environment), err
}

// buildEmbeddingBlock resolves the referenced Model and its credentials into
// the ingester's `embedding` block. When the credential is an Azure-identity
// (service principal) it returns extra env vars (AZURE_TENANT_ID/CLIENT_ID/
// CLIENT_SECRET) instead of an inline apiKey so the ingester can mint an AAD
// token at runtime — mirroring the dispatcher's model-credential handling.
func (l *KBIngesterLauncher) buildEmbeddingBlock(ctx context.Context, namespace, modelName string) (map[string]any, []corev1.EnvVar, error) {
	block, environment, err := l.payloadBuilder().BuildEmbeddingBlock(ctx, namespace, modelName)
	return block, toKubernetesEnv(environment), err
}

// buildSourcesBlock converts KB sources into the ingester's `sources` array.
// Sources that the ingester does not support yet (file, directory) are
// rejected loudly so the KB fails fast instead of silently producing an
// empty index.
func (l *KBIngesterLauncher) buildSourcesBlock(ctx context.Context, kb *api.KnowledgeBase) ([]map[string]any, error) {
	return l.payloadBuilder().BuildSourcesBlock(ctx, kb)
}

// readSecretRefKey resolves a SecretRef CR and returns the value of a single
// key from its internal-secret-data annotation.
func (l *KBIngesterLauncher) readSecretRefKey(ctx context.Context, namespace, name, key string) (string, error) {
	return l.payloadBuilder().ReadSecretRefKey(ctx, namespace, name, key)
}

// readSecretRefAll returns the full key/value map from a SecretRef's internal
// annotation, including credential metadata used to select an auth mode.
func (l *KBIngesterLauncher) readSecretRefAll(ctx context.Context, namespace, name string) (map[string]string, error) {
	return l.payloadBuilder().ReadSecretRefAll(ctx, namespace, name)
}

func (l *KBIngesterLauncher) payloadBuilder() *ingestion.PayloadBuilder {
	return ingestion.NewPayloadBuilder(l.store, func(ctx context.Context, namespace, name string) (map[string]string, error) {
		configMap := &corev1.ConfigMap{}
		if err := l.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, configMap); err != nil {
			return nil, err
		}
		return configMap.Data, nil
	})
}

func toKubernetesEnv(environment []ingestion.EnvVar) []corev1.EnvVar {
	result := make([]corev1.EnvVar, len(environment))
	for index, variable := range environment {
		result[index] = corev1.EnvVar{Name: variable.Name, Value: variable.Value}
	}
	return result
}

// ingestJobName builds a DNS-1123-compliant Job name that is unique per (KB,
// generation) so Refreshes don't collide with running Jobs.
func ingestJobName(kbName string, generation int64) string {
	name := fmt.Sprintf("kb-ingest-%s-%s", kbName, strconv.FormatInt(generation, 10))
	// Kubernetes limit: 63 chars for a resource name. Truncate the KB name
	// portion if needed while preserving the suffix.
	if len(name) <= validation.DNS1123LabelMaxLength {
		return name
	}
	suffix := "-" + strconv.FormatInt(generation, 10)
	keep := validation.DNS1123LabelMaxLength - len("kb-ingest-") - len(suffix)
	if keep < 1 {
		keep = 1
	}
	if keep > len(kbName) {
		keep = len(kbName)
	}
	return "kb-ingest-" + kbName[:keep] + suffix
}

// kbOwnerRef builds an OwnerReference so the Job is garbage-collected with
// the KB.
func kbOwnerRef(kb *api.KnowledgeBase) metav1.OwnerReference {
	blockOwner := true
	controller := true
	return metav1.OwnerReference{
		APIVersion:         "kognition.dev/v1alpha1",
		Kind:               "KnowledgeBase",
		Name:               kb.ObjectMeta.Name,
		UID:                types.UID(kb.ObjectMeta.UID),
		BlockOwnerDeletion: &blockOwner,
		Controller:         &controller,
	}
}

// indexRunOwnerRef builds an OwnerReference so the Job is garbage-collected
// with the KnowledgeBaseIndexRun that requested it.
func indexRunOwnerRef(run *api.KnowledgeBaseIndexRun) metav1.OwnerReference {
	blockOwner := true
	controller := true
	return metav1.OwnerReference{
		APIVersion:         "kognition.dev/v1alpha1",
		Kind:               "KnowledgeBaseIndexRun",
		Name:               run.ObjectMeta.Name,
		UID:                types.UID(run.ObjectMeta.UID),
		BlockOwnerDeletion: &blockOwner,
		Controller:         &controller,
	}
}

func hasControllerOwner(refs []metav1.OwnerReference, kind, name, uid string) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.Kind == kind && ref.Name == name && string(ref.UID) == uid {
			return true
		}
	}
	return false
}

// indexRunJobName builds a DNS-1123-compliant Job name unique per IndexRun.
func indexRunJobName(runName string) string {
	name := "kb-indexrun-" + runName
	if len(name) <= validation.DNS1123LabelMaxLength {
		return name
	}
	prefix := "kb-indexrun-"
	digest := sha256.Sum256([]byte(runName))
	suffix := fmt.Sprintf("-%x", digest[:4])
	keep := validation.DNS1123LabelMaxLength - len(prefix) - len(suffix)
	if keep < 1 {
		keep = 1
	}
	if keep > len(runName) {
		keep = len(runName)
	}
	return prefix + runName[:keep] + suffix
}

// LaunchRun implements intctrl.IndexRunLauncher. It creates (idempotently) a
// one-shot ingest Job named for the run and owned by it, so deleting the run
// garbage-collects the Job.
func (l *KBIngesterLauncher) LaunchRun(
	ctx context.Context,
	kb *api.KnowledgeBase,
	kbc *api.KnowledgeBaseClass,
	run *api.KnowledgeBaseIndexRun,
) (intctrl.IngestState, intctrl.IngestStats, string, error) {
	if l.image == "" {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, "", nil
	}
	if kb.ObjectMeta.Namespace != "" && kb.ObjectMeta.Namespace == l.namespace {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, "", fmt.Errorf("ingestion workloads cannot use installation namespace %q; create the KnowledgeBase in a separate enrolled workload namespace", l.namespace)
	}

	jobName := indexRunJobName(run.ObjectMeta.Name)

	existing := &batchv1.Job{}
	err := l.client.Get(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName}, existing)
	switch {
	case err == nil:
		if !hasControllerOwner(existing.OwnerReferences, "KnowledgeBaseIndexRun", run.ObjectMeta.Name, run.ObjectMeta.UID) {
			return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("existing indexrun job %s/%s is not controlled by KnowledgeBaseIndexRun %s", kb.ObjectMeta.Namespace, jobName, run.ObjectMeta.Name)
		}
		state, stats, ierr := l.interpretJob(ctx, existing)
		if ierr == nil && state == intctrl.IngestRunning && kbc != nil {
			if err := l.ensureRunningJobInput(ctx, kb, kbc, jobName, indexRunOwnerRef(run)); err != nil {
				return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("ensure indexrun input secret: %w", err)
			}
		}
		return state, stats, jobName, ierr
	case apierrors.IsNotFound(err):
		if kbc == nil {
			return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("cannot create indexrun job after KnowledgeBaseClass deletion")
		}
	default:
		return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("get indexrun job: %w", err)
	}

	artifacts, err := l.buildArtifacts(ctx, kb, kbc, jobName)
	if err != nil {
		return intctrl.IngestFailed, intctrl.IngestStats{Error: err.Error()}, jobName, nil
	}
	if len(artifacts.payload) > ingestInputMaxSize {
		return intctrl.IngestFailed, intctrl.IngestStats{Error: fmt.Sprintf("ingest input exceeds %d bytes", ingestInputMaxSize)}, jobName, nil
	}

	secret := l.buildInputSecretWithProviderData(kb, jobName, artifacts.payload, artifacts.extraEnv, artifacts.providerData)
	secret.OwnerReferences = []metav1.OwnerReference{indexRunOwnerRef(run)}
	if err := l.ensureInputSecret(ctx, secret); err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("create indexrun input secret: %w", err)
	}
	job := l.buildJob(kb, jobName, secret.Name, artifacts.extraEnv)
	if artifacts.providerData != nil {
		configureProviderJob(job, secret.Name, l.provider.Name)
	}
	// Re-parent to the IndexRun so lifecycle is bound to the run, not the KB.
	job.OwnerReferences = []metav1.OwnerReference{indexRunOwnerRef(run)}
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels["kognition.dev/indexrun"] = run.ObjectMeta.Name
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = map[string]string{}
	}
	job.Spec.Template.Labels["kognition.dev/indexrun"] = run.ObjectMeta.Name

	if err := l.client.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := l.client.Get(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: jobName}, existing); err != nil {
				return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("get raced indexrun job: %w", err)
			}
			if !hasControllerOwner(existing.OwnerReferences, "KnowledgeBaseIndexRun", run.ObjectMeta.Name, run.ObjectMeta.UID) {
				return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("existing indexrun job %s/%s is not controlled by KnowledgeBaseIndexRun %s", kb.ObjectMeta.Namespace, jobName, run.ObjectMeta.Name)
			}
			state, stats, ierr := l.interpretJob(ctx, existing)
			return state, stats, jobName, ierr
		}
		return intctrl.IngestSkipped, intctrl.IngestStats{}, jobName, fmt.Errorf("create indexrun job: %w", err)
	}
	l.logger.Info("launched kb indexrun job",
		zap.String("job", jobName),
		zap.String("kb", kb.ObjectMeta.Name),
		zap.String("run", run.ObjectMeta.Name),
		zap.String("namespace", kb.ObjectMeta.Namespace),
	)
	return intctrl.IngestRunning, intctrl.IngestStats{}, jobName, nil
}

// Ensure imports pulled in by builds elsewhere in the package don't dead-code.
var _ = k8s.KnowledgeBase{}
var _ = convert.KnowledgeBaseToK8s
