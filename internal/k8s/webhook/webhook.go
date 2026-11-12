// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package webhook provides a controller-runtime admission webhook handler that
// delegates to the internal admission.Chain for mutating and validating K8s resources.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
)

func admissionIdentityExtra(values map[string]authenticationv1.ExtraValue) map[string][]string {
	result := make(map[string][]string, len(values))
	for key, value := range values {
		result[key] = append([]string(nil), value...)
	}
	return result
}

// Handler implements admission.Handler by delegating to the internal admission.Chain.
// It converts K8s CRD objects to internal types, runs the chain, and produces
// a JSON patch if mutation occurred.
type Handler struct {
	chain                        *intadmission.Chain
	decoder                      admission.Decoder
	logger                       *zap.Logger
	mode                         handlerMode
	auditLog                     *observability.CognitiveAuditLog
	runtimeApprovalDecisionStore runtimeApprovalOperationStore
	runtimeApprovalForbidden     []string
}

type runtimeApprovalOperationStore interface {
	runtimeApprovalDecisionStore
	runtimeApprovalReconciliationStore
}

// AuditHandler records a resource mutation from admission metadata without
// requiring a typed conversion for the resource being audited.
type AuditHandler struct {
	auditLog *observability.CognitiveAuditLog
	logger   *zap.Logger
}

type handlerMode uint8

const (
	modeCombined handlerMode = iota
	modeMutate
	modeValidate
)

// NewHandler creates a webhook handler wrapping the given admission chain.
func NewHandler(chain *intadmission.Chain, scheme *runtime.Scheme, logger *zap.Logger, options ...Option) *Handler {
	handler := &Handler{
		chain:   chain,
		decoder: admission.NewDecoder(scheme),
		logger:  logger,
		mode:    modeCombined,
	}
	for _, option := range options {
		option(handler)
	}
	return handler
}

// Option configures an admission Handler.
type Option func(*Handler)

// WithAuditLog persists a resource mutation before the API server commits it.
func WithAuditLog(auditLog *observability.CognitiveAuditLog) Option {
	return func(handler *Handler) { handler.auditLog = auditLog }
}

// WithRuntimeApprovalDecisionStore enables the authenticated decision and reconciliation operations.
func WithRuntimeApprovalDecisionStore(decisionStore runtimeApprovalOperationStore, forbiddenSubjects ...string) Option {
	return func(handler *Handler) {
		handler.runtimeApprovalDecisionStore = decisionStore
		handler.runtimeApprovalForbidden = append([]string(nil), forbiddenSubjects...)
	}
}

// NewAuditHandler creates the generic write-ahead mutation audit handler.
func NewAuditHandler(auditLog *observability.CognitiveAuditLog, logger *zap.Logger) *AuditHandler {
	return &AuditHandler{auditLog: auditLog, logger: logger}
}

func newHandlerForMode(chain *intadmission.Chain, scheme *runtime.Scheme, logger *zap.Logger, mode handlerMode, options ...Option) *Handler {
	handler := NewHandler(chain, scheme, logger, options...)
	handler.mode = mode
	return handler
}

// Handle implements admission.Handler.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	ctx = requestid.NewContext(ctx, string(req.UID))

	op, err := mapOperation(req.Operation)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	kind := req.Kind.Kind

	objectRaw := req.Object.Raw
	if req.Operation == admissionv1.Delete {
		objectRaw = req.OldObject.Raw
	}
	// Convert the current object, or the deleted object for DELETE requests,
	// to the internal representation consumed by the admission chain.
	internalJSON, err := h.convertToInternal(kind, objectRaw)
	if err != nil {
		h.logger.Error("failed to convert object to internal type",
			zap.String("kind", kind), zap.Error(err), requestid.ZapField(ctx))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Build internal admission request.
	admReq := &intadmission.Request{
		Operation: op,
		Kind:      kind,
		Namespace: req.Namespace,
		Name:      req.Name,
		Object:    internalJSON,
		UserInfo: intadmission.UserInfo{
			Username: req.UserInfo.Username,
			Groups:   req.UserInfo.Groups,
			Extra:    admissionIdentityExtra(req.UserInfo.Extra),
		},
	}

	// Convert OldObject if present (UPDATE operations).
	if req.OldObject.Raw != nil {
		oldJSON, err := h.convertToInternal(kind, req.OldObject.Raw)
		if err != nil {
			h.logger.Error("failed to convert old object to internal type",
				zap.String("kind", kind), zap.Error(err), requestid.ZapField(ctx))
			return admission.Errored(http.StatusInternalServerError, err)
		}
		admReq.OldObject = oldJSON
	}

	var mutatedJSON json.RawMessage
	switch h.mode {
	case modeMutate:
		mutatedJSON, err = h.chain.Mutate(ctx, admReq)
	case modeValidate:
		err = h.chain.Validate(ctx, admReq)
	default:
		mutatedJSON, err = h.chain.Admit(ctx, admReq)
	}
	if err != nil {
		var denied *intadmission.DeniedError
		if errors.As(err, &denied) {
			return admission.Denied(denied.Reason)
		}
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// If the chain produced a mutated object, convert back to K8s and create a patch.
	if h.mode != modeValidate && mutatedJSON != nil && string(mutatedJSON) != string(internalJSON) {
		patchedRaw, err := h.convertToK8s(kind, mutatedJSON, req.Object.Raw)
		if err != nil {
			h.logger.Error("failed to convert mutated object back to K8s",
				zap.String("kind", kind), zap.Error(err), requestid.ZapField(ctx))
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.Object.Raw, patchedRaw)
	}

	return admission.Allowed("")
}

// Handle records the requested mutation before the API server persists it.
func (h *AuditHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("")
	}
	op, err := mapOperation(req.Operation)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := recordResourceAudit(ctx, h.auditLog, req, req.Kind.Kind, op); err != nil {
		h.logger.Error("failed to persist Kubernetes resource audit entry",
			zap.String("kind", req.Kind.Kind), zap.String("namespace", req.Namespace), zap.String("name", req.Name), zap.Error(err), requestid.ZapField(ctx))
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.Allowed("")
}

// SetupWithManager registers the webhook handler with the given manager's webhook server.
// It registers phase-specific handlers for Kubernetes mutation and validation.
func SetupWithManager(mgr ctrl.Manager, chain *intadmission.Chain, logger *zap.Logger, options ...Option) error {
	scheme := mgr.GetScheme()
	configured := NewHandler(chain, scheme, logger, options...)

	srv := mgr.GetWebhookServer()
	srv.Register("/mutate", &webhook.Admission{Handler: newHandlerForMode(chain, scheme, logger, modeMutate)})
	srv.Register("/validate", &webhook.Admission{Handler: newHandlerForMode(chain, scheme, logger, modeValidate)})
	if configured.auditLog != nil {
		srv.Register("/audit", &webhook.Admission{Handler: NewAuditHandler(configured.auditLog, logger)})
	}
	if configured.runtimeApprovalDecisionStore != nil {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return fmt.Errorf("create Kubernetes authorization client: %w", err)
		}
		srv.Register(RuntimeApprovalDecisionPath, NewRuntimeApprovalDecisionHandler(
			clientset.AuthenticationV1().TokenReviews(),
			clientset.AuthorizationV1().SubjectAccessReviews(),
			configured.runtimeApprovalDecisionStore,
			configured.runtimeApprovalForbidden...,
		))
		srv.Register(RuntimeApprovalReconciliationPath, NewRuntimeApprovalReconciliationHandler(
			clientset.AuthenticationV1().TokenReviews(),
			clientset.AuthorizationV1().SubjectAccessReviews(),
			configured.runtimeApprovalDecisionStore,
			configured.runtimeApprovalForbidden...,
		))
	}

	return nil
}

func recordResourceAudit(ctx context.Context, auditLog *observability.CognitiveAuditLog, req admission.Request, kind string, operation intadmission.Operation) error {
	if auditLog == nil {
		return nil
	}
	workspace := req.Namespace
	if workspace == "" {
		workspace = "cluster"
	}
	details, err := json.Marshal(map[string]string{"phase": "admission", "outcome": "attempt"})
	if err != nil {
		return fmt.Errorf("marshal admission audit details: %w", err)
	}
	return auditLog.RecordBeforeExec(ctx, observability.AuditEntry{
		Workspace:     workspace,
		AgentIdentity: req.UserInfo.Username,
		EventType:     observability.AuditAdmissionAttempt,
		SubjectKind:   kind,
		SubjectRef:    req.Namespace + "/" + req.Name,
		Verb:          "attempt_" + strings.ToLower(string(operation)),
		Details:       details,
	})
}

// mapOperation converts a K8s admission operation to the internal Operation type.
func mapOperation(op admissionv1.Operation) (intadmission.Operation, error) {
	switch op {
	case admissionv1.Create:
		return intadmission.OperationCreate, nil
	case admissionv1.Update:
		return intadmission.OperationUpdate, nil
	case admissionv1.Delete:
		return intadmission.OperationDelete, nil
	default:
		return "", fmt.Errorf("unsupported admission operation: %s", op)
	}
}

// convertToInternal converts raw K8s CRD JSON to internal type JSON.
func (h *Handler) convertToInternal(kind string, raw []byte) (json.RawMessage, error) {
	switch kind {
	case "Agent":
		var obj k8s.Agent
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentFromK8s(&obj)
		return json.Marshal(internal)
	case "AgentInstance":
		var obj k8s.AgentInstance
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentInstanceFromK8s(&obj)
		return json.Marshal(internal)
	case "AgentPolicy":
		var obj k8s.AgentPolicy
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentPolicyFromK8s(&obj)
		return json.Marshal(internal)
	case "AutonomyReview":
		var obj k8s.AutonomyReview
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AutonomyReviewFromK8s(&obj)
		return json.Marshal(internal)
	case "TrustProfile":
		var obj k8s.TrustProfile
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.TrustProfileFromK8s(&obj))
	case "RuntimeApproval":
		var obj k8s.RuntimeApproval
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.RuntimeApprovalFromK8s(&obj))
	case "CognitiveAdmissionPolicy":
		var obj k8s.CognitiveAdmissionPolicy
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.CognitiveAdmissionPolicyFromK8s(&obj)
		return json.Marshal(internal)
	case "CognitiveQuota":
		var obj k8s.CognitiveQuota
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.CognitiveQuotaFromK8s(&obj)
		return json.Marshal(internal)
	case "CognitiveLimitRange":
		var obj k8s.CognitiveLimitRange
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.CognitiveLimitRangeFromK8s(&obj)
		return json.Marshal(internal)
	case "Workspace":
		var obj k8s.Workspace
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.WorkspaceFromK8s(&obj)
		return json.Marshal(internal)
	case "AgentIdentity":
		var obj k8s.AgentIdentity
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentIdentityFromK8s(&obj)
		return json.Marshal(internal)
	case "AgentRole":
		var obj k8s.AgentRole
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentRoleFromK8s(&obj)
		return json.Marshal(internal)
	case "AgentRoleBinding":
		var obj k8s.AgentRoleBinding
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.AgentRoleBindingFromK8s(&obj)
		return json.Marshal(internal)
	case "MemoryClass":
		var obj k8s.MemoryClass
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.MemoryClassFromK8s(&obj)
		return json.Marshal(internal)
	case "MemoryClaim":
		var obj k8s.MemoryClaim
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.MemoryClaimFromK8s(&obj)
		return json.Marshal(internal)
	case "MemoryVolume":
		var obj k8s.MemoryVolume
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.MemoryVolumeFromK8s(&obj)
		return json.Marshal(internal)
	case "ModelClass":
		var obj k8s.ModelClass
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.ModelClassFromK8s(&obj)
		return json.Marshal(internal)
	case "AutonomyClass":
		var obj k8s.AutonomyClass
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.AutonomyClassFromK8s(&obj))
	case "Model":
		var obj k8s.Model
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.ModelFromK8s(&obj))
	case "ModelInvocationProfile":
		var obj k8s.ModelInvocationProfile
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.ModelInvocationProfileFromK8s(&obj))
	case "SecretRef":
		var obj k8s.SecretRef
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.SecretRefFromK8s(&obj))
	case "Tool":
		var obj k8s.Tool
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.ToolFromK8s(&obj)
		return json.Marshal(internal)
	case "ToolClass":
		var obj k8s.ToolClass
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.ToolClassFromK8s(&obj)
		return json.Marshal(internal)
	case "Workflow":
		var obj k8s.Workflow
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.WorkflowFromK8s(&obj)
		return json.Marshal(internal)
	case "WorkflowRun":
		var obj k8s.WorkflowRun
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.WorkflowRunFromK8s(&obj)
		return json.Marshal(internal)
	case "ApprovalDecision":
		var obj k8s.ApprovalDecision
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		internal := convert.ApprovalDecisionFromK8s(&obj)
		return json.Marshal(internal)
	case "KnowledgeBaseClass":
		var obj k8s.KnowledgeBaseClass
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.KnowledgeBaseClassFromK8s(&obj))
	case "KnowledgeBase":
		var obj k8s.KnowledgeBase
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.KnowledgeBaseFromK8s(&obj))
	case "KnowledgeBaseClaim":
		var obj k8s.KnowledgeBaseClaim
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.KnowledgeBaseClaimFromK8s(&obj))
	case "KnowledgeBaseIndexRun":
		var obj k8s.KnowledgeBaseIndexRun
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.KnowledgeBaseIndexRunFromK8s(&obj))
	case "KnowledgeBaseIndexSchedule":
		var obj k8s.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", kind, err)
		}
		return json.Marshal(convert.KnowledgeBaseIndexScheduleFromK8s(&obj))
	default:
		return nil, fmt.Errorf("unsupported kind for webhook: %s", kind)
	}
}

// convertToK8s converts a mutated internal JSON back to K8s CRD JSON.
// It preserves the original K8s metadata by unmarshalling the mutated internal type,
// converting to K8s, and marshalling back.
func (h *Handler) convertToK8s(kind string, mutatedJSON json.RawMessage, originalRaw []byte) ([]byte, error) {
	switch kind {
	case "Agent":
		var obj k8s.Agent
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "TrustProfile":
		var obj k8s.TrustProfile
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.TrustProfileFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.TrustProfileToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "RuntimeApproval":
		var obj k8s.RuntimeApproval
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.RuntimeApprovalFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.RuntimeApprovalToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AgentInstance":
		var obj k8s.AgentInstance
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentInstanceFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentInstanceToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "Tool":
		var obj k8s.Tool
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ToolFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.ToolToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "ToolClass":
		var obj k8s.ToolClass
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ToolClassFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.ToolClassToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AgentPolicy":
		var obj k8s.AgentPolicy
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentPolicyFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentPolicyToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "CognitiveAdmissionPolicy":
		var obj k8s.CognitiveAdmissionPolicy
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.CognitiveAdmissionPolicyFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.CognitiveAdmissionPolicyToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "CognitiveQuota":
		var obj k8s.CognitiveQuota
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.CognitiveQuotaFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.CognitiveQuotaToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "CognitiveLimitRange":
		var obj k8s.CognitiveLimitRange
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.CognitiveLimitRangeFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.CognitiveLimitRangeToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "Workspace":
		var obj k8s.Workspace
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.WorkspaceFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.WorkspaceToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AgentIdentity":
		var obj k8s.AgentIdentity
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentIdentityFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentIdentityToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AgentRole":
		var obj k8s.AgentRole
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentRoleFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentRoleToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AgentRoleBinding":
		var obj k8s.AgentRoleBinding
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AgentRoleBindingFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AgentRoleBindingToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "MemoryClass":
		var obj k8s.MemoryClass
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.MemoryClassFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.MemoryClassToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "MemoryClaim":
		var obj k8s.MemoryClaim
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.MemoryClaimFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.MemoryClaimToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "MemoryVolume":
		var obj k8s.MemoryVolume
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.MemoryVolumeFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.MemoryVolumeToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "ModelClass":
		var obj k8s.ModelClass
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ModelClassFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.ModelClassToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "AutonomyClass":
		var obj k8s.AutonomyClass
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.AutonomyClassFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.AutonomyClassToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "Model":
		var obj k8s.Model
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ModelFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.ModelToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "ModelInvocationProfile":
		var obj k8s.ModelInvocationProfile
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ModelInvocationProfileFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, internal); err != nil {
			return nil, err
		}
		patched := convert.ModelInvocationProfileToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "SecretRef":
		var obj k8s.SecretRef
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.SecretRefFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.SecretRefToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "Workflow":
		var obj k8s.Workflow
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.WorkflowFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.WorkflowToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "WorkflowRun":
		var obj k8s.WorkflowRun
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.WorkflowRunFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.WorkflowRunToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "ApprovalDecision":
		var obj k8s.ApprovalDecision
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.ApprovalDecisionFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.ApprovalDecisionToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "KnowledgeBaseClass":
		var obj k8s.KnowledgeBaseClass
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.KnowledgeBaseClassFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.KnowledgeBaseClassToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "KnowledgeBase":
		var obj k8s.KnowledgeBase
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.KnowledgeBaseFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.KnowledgeBaseToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "KnowledgeBaseClaim":
		var obj k8s.KnowledgeBaseClaim
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.KnowledgeBaseClaimFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.KnowledgeBaseClaimToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "KnowledgeBaseIndexRun":
		var obj k8s.KnowledgeBaseIndexRun
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.KnowledgeBaseIndexRunFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.KnowledgeBaseIndexRunToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	case "KnowledgeBaseIndexSchedule":
		var obj k8s.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(originalRaw, &obj); err != nil {
			return nil, err
		}
		internal := convert.KnowledgeBaseIndexScheduleFromK8s(&obj)
		if err := json.Unmarshal(mutatedJSON, &internal); err != nil {
			return nil, err
		}
		patched := convert.KnowledgeBaseIndexScheduleToK8s(internal)
		patched.TypeMeta = obj.TypeMeta
		patched.ObjectMeta = mergeMutatedMeta(obj.ObjectMeta, patched.ObjectMeta)
		return json.Marshal(patched)
	default:
		return nil, fmt.Errorf("convertToK8s not implemented for kind: %s", kind)
	}
}

// mergeMutatedMeta preserves identity fields from the original ObjectMeta
// while overlaying mutator-driven fields (Labels, Annotations, Finalizers)
// from the mutated ObjectMeta. Without this, label/annotation mutations
// performed by the admission chain would be silently dropped.
func mergeMutatedMeta(base, mutated metav1.ObjectMeta) metav1.ObjectMeta {
	base.Labels = mutated.Labels
	base.Annotations = mutated.Annotations
	base.Finalizers = mutated.Finalizers
	return base
}
