// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type WorkflowUseAccessReviewer interface {
	Create(context.Context, *authorizationv1.SubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
}

type WorkflowUseAdmissionAuthorizer struct {
	Reader        client.Reader
	AccessReviews WorkflowUseAccessReviewer
}

func (authorizer *WorkflowUseAdmissionAuthorizer) Authorize(ctx context.Context, submitter authenticationv1.UserInfo, reference api.RuntimeApprovalReference, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if authorizer == nil || authorizer.Reader == nil || authorizer.AccessReviews == nil {
		return runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || len(decodedDigest) != 32 || strings.ToLower(digest) != digest || reference.Kind != "Workflow" || reference.Scope != "Namespaced" ||
		len(validation.IsDNS1123Label(reference.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(reference.Name)) != 0 ||
		reference.UID == "" || reference.ResourceVersion < 1 || reference.ConfigurationDigest != "" {
		return controlplaneauth.ErrForbidden
	}
	validValue := func(value string, limit int) bool {
		return value != "" && len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
	}
	if !validValue(submitter.Username, 256) || submitter.Username == "system:anonymous" ||
		submitter.UID != "" && !validValue(submitter.UID, 256) || len(submitter.Groups) > 64 || len(submitter.Extra) > 16 {
		return controlplaneauth.ErrForbidden
	}
	groups := make([]string, 0, len(submitter.Groups))
	for _, group := range submitter.Groups {
		if !validValue(group, 256) || group == "system:unauthenticated" {
			return controlplaneauth.ErrForbidden
		}
		groups = append(groups, group)
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(submitter.Extra))
	for key, values := range submitter.Extra {
		if !validValue(key, 256) || len(values) > 16 {
			return controlplaneauth.ErrForbidden
		}
		for _, value := range values {
			if !validValue(value, 256) {
				return controlplaneauth.ErrForbidden
			}
		}
		extra[key] = append(authorizationv1.ExtraValue(nil), values...)
	}
	if err := authorizer.validateDefinition(ctx, reference, digest); err != nil {
		return err
	}
	review, err := authorizer.AccessReviews.Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: submitter.Username, UID: submitter.UID, Groups: groups, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: reference.Namespace, Group: "kognition.dev", Resource: "workflows", Name: reference.Name, Verb: "use"},
	}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Workflow/use authorization unavailable: %w", controlplaneauth.ErrUnavailable)
	}
	if review == nil || review.Status.EvaluationError != "" {
		return controlplaneauth.ErrUnavailable
	}
	if !review.Status.Allowed || review.Status.Denied {
		return controlplaneauth.ErrForbidden
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return authorizer.validateDefinition(ctx, reference, digest)
}

func (authorizer *WorkflowUseAdmissionAuthorizer) validateDefinition(ctx context.Context, reference api.RuntimeApprovalReference, digest string) error {
	workflow := &k8s.Workflow{}
	if err := authorizer.Reader.Get(ctx, client.ObjectKey{Namespace: reference.Namespace, Name: reference.Name}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return controlplaneauth.ErrForbidden
		}
		return controlplaneauth.ErrUnavailable
	}
	if workflow.Namespace != reference.Namespace || workflow.Name != reference.Name || string(workflow.UID) != reference.UID ||
		workflow.ResourceVersion != strconv.FormatInt(reference.ResourceVersion, 10) || workflow.DeletionTimestamp != nil {
		return controlplaneauth.ErrForbidden
	}
	raw, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		return controlplaneauth.ErrUnavailable
	}
	currentDigest, err := runtimeapproval.WorkflowExecutorSpecDigest(raw)
	if err != nil || currentDigest != digest {
		return controlplaneauth.ErrForbidden
	}
	return ctx.Err()
}

type WorkflowSubmissionBindingReader interface {
	GetWorkflowExecutorAuthority(context.Context, api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error)
}

type WorkflowRunSubmissionService struct {
	Authorizer      *WorkflowUseAdmissionAuthorizer
	Bindings        WorkflowSubmissionBindingReader
	Keys            runtimeapproval.BindingKeyStore
	Receipts        runtimeapproval.NativeWorkflowSubmissionReceiptStore
	Clock           func() (time.Time, error)
	RecordNamespace string
	CurrentWriter   func(context.Context) (authenticationv1.UserInfo, error)
}

func (service *WorkflowRunSubmissionService) authorizeRecordWriter(ctx context.Context, request admission.Request, authority runtimeapproval.WorkflowExecutorAuthority) error {
	if service == nil || service.CurrentWriter == nil || service.RecordNamespace == "" || request.Namespace != service.RecordNamespace ||
		request.Operation != admissionv1.Create || request.DryRun != nil && *request.DryRun || request.UserInfo.Username == "" || request.UserInfo.Username == "system:anonymous" || len(request.UserInfo.Groups) > 64 {
		return controlplaneauth.ErrForbidden
	}
	for _, group := range request.UserInfo.Groups {
		if group == "system:unauthenticated" {
			return controlplaneauth.ErrForbidden
		}
	}
	user, err := service.CurrentWriter(ctx)
	if err != nil || user.Username != authority.Executor.Subject || user.Username != request.UserInfo.Username || user.UID == "" || user.UID != request.UserInfo.UID ||
		!strings.HasPrefix(user.Username, "system:serviceaccount:") {
		return controlplaneauth.ErrForbidden
	}
	for _, key := range []string{"authentication.kubernetes.io/pod-name", "authentication.kubernetes.io/pod-uid"} {
		if len(user.Extra[key]) != 1 || user.Extra[key][0] == "" || !reflect.DeepEqual(user.Extra[key], request.UserInfo.Extra[key]) {
			return controlplaneauth.ErrForbidden
		}
	}
	for _, controllerUID := range authority.ControllerUIDs {
		if controllerUID == user.Extra["authentication.kubernetes.io/pod-uid"][0] {
			return ctx.Err()
		}
	}
	return controlplaneauth.ErrForbidden
}

func (service *WorkflowRunSubmissionService) AuthorizePrivateRecordWrite(ctx context.Context, request admission.Request, submission bool) error {
	if service == nil || request.Operation != admissionv1.Create || len(request.Object.Raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return controlplaneauth.ErrForbidden
	}
	if _, err := runtimeapproval.CanonicalizeJSON(request.Object.Raw); err != nil {
		return controlplaneauth.ErrForbidden
	}
	var object corev1.ConfigMap
	if json.Unmarshal(request.Object.Raw, &object) != nil || object.Name != request.Name || object.Namespace != request.Namespace || object.GenerateName != "" ||
		object.DeletionTimestamp != nil || object.Immutable == nil || !*object.Immutable || len(object.Data) != 1 || len(object.BinaryData) != 0 || len(object.OwnerReferences) != 0 {
		return controlplaneauth.ErrForbidden
	}
	var reference api.RuntimeApprovalReference
	var receipt runtimeapproval.NativeWorkflowSubmissionReceipt
	var acceptance runtimeapproval.WorkflowRunAcceptance
	if submission {
		var err error
		receipt, err = runtimeapproval.DecodeNativeWorkflowSubmissionReceipt([]byte(object.Data["receipt.json"]))
		if err != nil || object.Labels["kognition.dev/internal-record"] != "workflow-run-submission" ||
			object.Name != runtimeapproval.NativeWorkflowSubmissionObjectName(receipt.Run.Namespace, receipt.Run.UID) {
			return controlplaneauth.ErrForbidden
		}
		reference = receipt.ExecutionBinding
	} else {
		var err error
		acceptance, err = runtimeapproval.DecodeWorkflowRunAcceptance([]byte(object.Data["acceptance.json"]))
		if err != nil || object.Labels["kognition.dev/internal-record"] != "workflow-run-acceptance" ||
			object.Name != runtimeapproval.WorkflowRunAcceptanceObjectName(acceptance.Workspace, acceptance.RunUID) {
			return controlplaneauth.ErrForbidden
		}
		reference = acceptance.ExecutionBinding
	}
	authority, now, err := service.authority(ctx, reference)
	if err != nil {
		return err
	}
	if err := service.authorizeRecordWriter(ctx, request, authority); err != nil {
		return err
	}
	if submission {
		if receipt.Workflow != authority.Workflow || receipt.WorkflowRevisionDigest != authority.WorkflowRevisionDigest || receipt.Executor != authority.Executor ||
			runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, now) != nil {
			return controlplaneauth.ErrForbidden
		}
		if err := service.Authorizer.Authorize(ctx, receipt.Submitter, receipt.Workflow, receipt.WorkflowRevisionDigest); err != nil {
			return err
		}
	} else {
		run := &k8s.WorkflowRun{}
		if err := service.Authorizer.Reader.Get(ctx, client.ObjectKey{Namespace: acceptance.Workspace, Name: acceptance.Run.Name}, run); err != nil {
			return controlplaneauth.ErrForbidden
		}
		raw, err := json.Marshal(convert.WorkflowRunFromK8s(run))
		if err != nil {
			return err
		}
		expected, err := service.AuthorizeAcceptance(ctx, raw)
		if err != nil || !reflect.DeepEqual(expected, acceptance) {
			return controlplaneauth.ErrForbidden
		}
	}
	current, finalNow, err := service.authority(ctx, reference)
	if err != nil || !reflect.DeepEqual(current, authority) || finalNow.Before(now) {
		return controlplaneauth.ErrForbidden
	}
	if submission && runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, finalNow) != nil {
		return controlplaneauth.ErrForbidden
	}
	return service.authorizeRecordWriter(ctx, request, current)
}

func (service *WorkflowRunSubmissionService) authority(ctx context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, time.Time, error) {
	if service == nil || service.Authorizer == nil || service.Authorizer.Reader == nil || service.Authorizer.AccessReviews == nil ||
		service.Bindings == nil || service.Keys == nil || service.Receipts == nil || service.Clock == nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, time.Time{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	if err := ctx.Err(); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, time.Time{}, err
	}
	authority, err := service.Bindings.GetWorkflowExecutorAuthority(ctx, reference)
	if err != nil {
		return authority, time.Time{}, err
	}
	now, err := service.Clock()
	if err != nil {
		return authority, time.Time{}, err
	}
	if runtimeapproval.ValidateWorkflowExecutorAuthority(authority) != nil || authority.Reference != reference ||
		authority.Executor.Method != "tokenreview" || authority.Executor.Issuer != "kubernetes.io/tokenreview" ||
		now.IsZero() || !authority.Enabled || now.Before(authority.NotBefore) || !now.Before(authority.ExpiresAt) {
		return authority, now, controlplaneauth.ErrForbidden
	}
	return authority, now, nil
}

func (service *WorkflowRunSubmissionService) Preflight(ctx context.Context, request admission.Request, reference api.RuntimeApprovalReference) error {
	if request.Operation != admissionv1.Create || request.Kind.Group != "kognition.dev" || request.Kind.Kind != "WorkflowRun" ||
		request.UID == "" || request.Name == "" || request.DryRun != nil && *request.DryRun || len(request.Object.Raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return controlplaneauth.ErrForbidden
	}
	if _, err := runtimeapproval.CanonicalizeJSON(request.Object.Raw); err != nil {
		return controlplaneauth.ErrForbidden
	}
	var native k8s.WorkflowRun
	if json.Unmarshal(request.Object.Raw, &native) != nil || native.Name != request.Name || native.Namespace != request.Namespace || native.UID != "" || native.ResourceVersion != "" ||
		native.Annotations[runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation] != "true" || native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" {
		return controlplaneauth.ErrForbidden
	}
	authority, _, err := service.authority(ctx, reference)
	if err != nil {
		return err
	}
	if native.Namespace != authority.Workflow.Namespace || native.Spec.WorkflowRef.Name != authority.Workflow.Name {
		return controlplaneauth.ErrForbidden
	}
	raw, err := json.Marshal(convert.WorkflowRunFromK8s(&native))
	if err != nil {
		return err
	}
	if err := validatePendingSubmissionRun(raw); err != nil {
		return err
	}
	return service.Authorizer.Authorize(ctx, request.UserInfo, authority.Workflow, authority.WorkflowRevisionDigest)
}

func (service *WorkflowRunSubmissionService) Issue(ctx context.Context, request admission.Request, reference api.RuntimeApprovalReference) (runtimeapproval.NativeWorkflowSubmissionReceipt, error) {
	if request.Operation != admissionv1.Create || request.Kind.Group != "kognition.dev" || request.Kind.Kind != "WorkflowRun" ||
		request.UID == "" || request.Name == "" || request.DryRun != nil && *request.DryRun || len(request.Object.Raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, controlplaneauth.ErrForbidden
	}
	if _, err := runtimeapproval.CanonicalizeJSON(request.Object.Raw); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, controlplaneauth.ErrForbidden
	}
	var native k8s.WorkflowRun
	if err := json.Unmarshal(request.Object.Raw, &native); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	if native.Name != request.Name || native.Namespace != request.Namespace || native.UID == "" || native.ResourceVersion != "" || native.DeletionTimestamp != nil ||
		native.Annotations[runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation] != "true" || native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, controlplaneauth.ErrForbidden
	}
	authority, now, err := service.authority(ctx, reference)
	if err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	if native.Namespace != authority.Workflow.Namespace || native.Spec.WorkflowRef.Name != authority.Workflow.Name {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, controlplaneauth.ErrForbidden
	}
	if err := service.Authorizer.Authorize(ctx, request.UserInfo, authority.Workflow, authority.WorkflowRevisionDigest); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	runJSON, err := json.Marshal(convert.WorkflowRunFromK8s(&native))
	if err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	if err := validatePendingSubmissionRun(runJSON); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	digest, err := runtimeapproval.WorkflowExecutorSpecDigest(runJSON)
	if err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	expires := now.Add(runtimeapproval.NativeWorkflowSubmissionTTL)
	if expires.After(authority.ExpiresAt) {
		expires = authority.ExpiresAt
	}
	receipt := runtimeapproval.NativeWorkflowSubmissionReceipt{AdmissionUID: string(request.UID),
		Run:      api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: native.Namespace, Name: native.Name, UID: string(native.UID)},
		Workflow: authority.Workflow, ExecutionBinding: authority.Reference, Executor: authority.Executor, WorkflowRevisionDigest: authority.WorkflowRevisionDigest,
		RequestDigest: digest, Submitter: *request.UserInfo.DeepCopy(), IssuedAt: now.UTC(), ExpiresAt: expires.UTC()}
	sealed, err := runtimeapproval.SealNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, now)
	if err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	current, finalNow, err := service.authority(ctx, reference)
	if err != nil || !reflect.DeepEqual(current, authority) || finalNow.Before(now) {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, controlplaneauth.ErrForbidden
	}
	if err := runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, sealed, finalNow); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	if err := service.Receipts.CreateNativeWorkflowSubmissionReceipt(ctx, sealed); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	if err := service.VerifyAdmission(ctx, request, reference); err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	return service.Receipts.GetNativeWorkflowSubmissionReceipt(ctx, native.Namespace, string(native.UID))
}

func validatePendingSubmissionRun(raw []byte) error {
	var run api.WorkflowRun
	if json.Unmarshal(raw, &run) != nil || run.DeletionTimestamp != nil || run.Status.CompletionTime != nil || run.Status.StartTime != nil ||
		run.Status.SnapshotSpec != nil || run.Status.ActiveStep != "" || len(run.Status.ActiveSteps) != 0 || len(run.Status.StepStatuses) != 0 ||
		run.Status.Phase != "" && run.Status.Phase != api.PhasePending {
		return controlplaneauth.ErrForbidden
	}
	return nil
}

func (service *WorkflowRunSubmissionService) VerifyAdmission(ctx context.Context, request admission.Request, reference api.RuntimeApprovalReference) error {
	if request.Operation != admissionv1.Create || request.Kind.Group != "kognition.dev" || request.Kind.Kind != "WorkflowRun" ||
		request.UID == "" || request.Name == "" || request.DryRun != nil && *request.DryRun || len(request.Object.Raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return controlplaneauth.ErrForbidden
	}
	if _, err := runtimeapproval.CanonicalizeJSON(request.Object.Raw); err != nil {
		return controlplaneauth.ErrForbidden
	}
	var native k8s.WorkflowRun
	if json.Unmarshal(request.Object.Raw, &native) != nil || native.Name != request.Name || native.Namespace != request.Namespace || native.UID == "" || native.ResourceVersion != "" ||
		native.Annotations[runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation] != "true" {
		return controlplaneauth.ErrForbidden
	}
	authority, now, err := service.authority(ctx, reference)
	if err != nil {
		return err
	}
	receipt, err := service.Receipts.GetNativeWorkflowSubmissionReceipt(ctx, native.Namespace, string(native.UID))
	if err != nil || native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" {
		return controlplaneauth.ErrForbidden
	}
	if err := runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, now); err != nil {
		return controlplaneauth.ErrForbidden
	}
	if receipt.Run.UID != string(native.UID) || receipt.Run.Namespace != native.Namespace || receipt.Run.Name != native.Name ||
		receipt.ExecutionBinding != reference || receipt.Workflow != authority.Workflow || receipt.WorkflowRevisionDigest != authority.WorkflowRevisionDigest ||
		receipt.Executor != authority.Executor || native.Spec.WorkflowRef.Name != receipt.Workflow.Name || !reflect.DeepEqual(receipt.Submitter, request.UserInfo) {
		return controlplaneauth.ErrForbidden
	}
	runJSON, err := json.Marshal(convert.WorkflowRunFromK8s(&native))
	if err != nil {
		return err
	}
	if err := validatePendingSubmissionRun(runJSON); err != nil {
		return err
	}
	digest, err := runtimeapproval.WorkflowExecutorSpecDigest(runJSON)
	if err != nil || digest != receipt.RequestDigest {
		return controlplaneauth.ErrForbidden
	}
	if err := service.Authorizer.Authorize(ctx, request.UserInfo, authority.Workflow, authority.WorkflowRevisionDigest); err != nil {
		return err
	}
	current, finalNow, err := service.authority(ctx, reference)
	if err != nil || !reflect.DeepEqual(current, authority) || finalNow.Before(now) {
		return controlplaneauth.ErrForbidden
	}
	return runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, finalNow)
}

func (service *WorkflowRunSubmissionService) AuthorizeAcceptance(ctx context.Context, runJSON []byte) (runtimeapproval.WorkflowRunAcceptance, error) {
	var run api.WorkflowRun
	if len(runJSON) > runtimeapproval.WorkflowExecutorRecordMaxBytes || json.Unmarshal(runJSON, &run) != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	if service == nil || service.Receipts == nil {
		return runtimeapproval.WorkflowRunAcceptance{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	receipt, err := service.Receipts.GetNativeWorkflowSubmissionReceipt(ctx, run.Namespace, run.UID)
	if err != nil || run.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	authority, now, err := service.authority(ctx, receipt.ExecutionBinding)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	if receipt.Workflow != authority.Workflow || receipt.WorkflowRevisionDigest != authority.WorkflowRevisionDigest || receipt.Executor != authority.Executor ||
		receipt.Run.Namespace != run.Namespace || receipt.Run.Name != run.Name || receipt.Run.UID != run.UID || run.ResourceVersion < 1 {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	expires := receipt.IssuedAt.Add(time.Duration(authority.Limits.MaxDurationSeconds) * time.Second)
	if expires.After(authority.ExpiresAt) {
		expires = authority.ExpiresAt
	}
	acceptance := runtimeapproval.WorkflowRunAcceptance{Workspace: run.Namespace, RunUID: run.UID,
		Run:      api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: run.Namespace, Name: run.Name, UID: run.UID, ResourceVersion: run.ResourceVersion},
		Workflow: receipt.Workflow, WorkflowRevisionDigest: receipt.WorkflowRevisionDigest, ExecutionBinding: receipt.ExecutionBinding, Executor: receipt.Executor,
		Initiator:     api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: receipt.Submitter.Username},
		RequestDigest: receipt.RequestDigest, AcceptedAt: receipt.IssuedAt, ExpiresAt: expires}
	acceptance.BindingDigest, err = runtimeapproval.WorkflowRunExecutionScopeDigest(acceptance, authority)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	workflow := &k8s.Workflow{}
	if err := service.Authorizer.Reader.Get(ctx, client.ObjectKey{Namespace: authority.Workflow.Namespace, Name: authority.Workflow.Name}, workflow); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	workflowJSON, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceEvidenceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := service.Authorizer.Authorize(ctx, receipt.Submitter, authority.Workflow, authority.WorkflowRevisionDigest); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	current, finalNow, err := service.authority(ctx, receipt.ExecutionBinding)
	if err != nil || !reflect.DeepEqual(current, authority) || finalNow.Before(now) {
		return runtimeapproval.WorkflowRunAcceptance{}, controlplaneauth.ErrForbidden
	}
	if err := runtimeapproval.VerifyNativeWorkflowSubmissionReceipt(ctx, service.Keys, receipt, finalNow); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	return acceptance, ctx.Err()
}
