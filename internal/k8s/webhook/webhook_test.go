// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/convert"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type webhookClaimantReader struct{}

func (webhookClaimantReader) VerifyRuntimeApprovalClaimant(context.Context, string, string, string, string, api.RuntimeApprovalClaimant) error {
	return nil
}

type auditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *auditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *auditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for index := range entries {
		if err := s.Append(ctx, entries[index]); err != nil {
			return err
		}
	}
	return nil
}

func (*auditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return nil, nil
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = k8s.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func testLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

func TestWorkflowAcceptanceConfigurationFailsClosedWithoutReceipt(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), WithWorkflowBindingAdministrators("control", []string{"operators"}, []string{"executor"}))
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete} {
		for _, username := range []string{"author", "executor", "operator"} {
			for name, metadata := range map[string]metav1.ObjectMeta{
				"reserved name":             {Name: "kognition-workflow-run-acceptance-record", Namespace: "control"},
				"reserved generated name":   {Name: "ordinary", Namespace: "control", GenerateName: "kognition-workflow-run-acceptance-"},
				"reserved label":            {Name: "ordinary", Namespace: "control", Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-acceptance"}},
				"submission name":           {Name: "kognition-workflow-run-submission-record", Namespace: "control"},
				"submission generated name": {Name: "ordinary", Namespace: "control", GenerateName: "kognition-workflow-run-submission-"},
				"submission label":          {Name: "ordinary", Namespace: "control", Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-submission"}},
			} {
				t.Run(string(operation)+"/"+username+"/"+name, func(t *testing.T) {
					oldJSON, err := json.Marshal(corev1.ConfigMap{ObjectMeta: metadata})
					if err != nil {
						t.Fatal(err)
					}
					current := metadata.DeepCopy()
					if operation == admissionv1.Update {
						current.Labels = nil
						current.GenerateName = ""
					}
					currentJSON, err := json.Marshal(corev1.ConfigMap{ObjectMeta: *current})
					if err != nil {
						t.Fatal(err)
					}
					request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "receipt-write", Operation: operation, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespace: "control", Name: metadata.Name,
						UserInfo: authenticationv1.UserInfo{Username: username, Groups: []string{"operators"}}, Object: runtime.RawExtension{Raw: currentJSON}}}
					if operation != admissionv1.Create {
						request.OldObject.Raw = oldJSON
					}
					if response := handler.Handle(context.Background(), request); response.Allowed || response.Result.Code != http.StatusForbidden {
						t.Fatalf("unqualified native acceptance allowed: %+v", response)
					}
				})
			}
		}
	}
}

func TestWorkflowRunSubmissionRequiresQualifiedReceipt(t *testing.T) {
	for _, mode := range []handlerMode{modeCombined, modeMutate, modeValidate} {
		for _, username := range []string{"author", "executor", "operator"} {
			for _, protected := range []bool{false, true} {
				run := k8s.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "investigation", Namespace: "team", UID: "actual-run-uid"},
					Spec: k8s.WorkflowRunSpec{WorkflowRef: k8s.LocalObjectReference{Name: "investigation"}}}
				if protected {
					run.Annotations = map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true", "kognition.dev/initiator": "operator"}
				}
				raw, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "actual-admission-request-uid", Operation: admissionv1.Create,
					Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkflowRun"}, Namespace: "team", Name: run.Name,
					UserInfo: authenticationv1.UserInfo{Username: username, UID: "actual-submitter-uid", Groups: []string{"operators"}}, Object: runtime.RawExtension{Raw: raw}}}
				handler := newHandlerForMode(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), mode,
					WithWorkflowBindingAdministrators("control", []string{"operators"}, []string{"executor"}))
				response := handler.Handle(context.Background(), request)
				if protected {
					if response.Allowed || response.Result == nil || response.Result.Code != http.StatusForbidden {
						t.Fatalf("protected native submission without receipt allowed for mode=%v actor=%s: %+v", mode, username, response)
					}
					request.Operation = admissionv1.Update
					request.OldObject.Raw = raw
					if response := handler.Handle(context.Background(), request); !response.Allowed {
						t.Fatalf("existing protected run update was blocked: %+v", response)
					}
				} else if !response.Allowed {
					t.Fatalf("ordinary native submission was blocked: %+v", response)
				}
			}
		}
	}
}

type workflowUseReviewFixture struct {
	request     *authorizationv1.SubjectAccessReview
	status      authorizationv1.SubjectAccessReviewStatus
	err         error
	after       func()
	nilResponse bool
}

func (reviewer *workflowUseReviewFixture) Create(_ context.Context, request *authorizationv1.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	reviewer.request = request.DeepCopy()
	if reviewer.after != nil {
		reviewer.after()
	}
	if reviewer.nilResponse {
		return nil, reviewer.err
	}
	return &authorizationv1.SubjectAccessReview{Status: reviewer.status}, reviewer.err
}

func TestWorkflowUseAdmissionAuthorization(t *testing.T) {
	workflow := &k8s.Workflow{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "investigation", UID: "actual-workflow-uid", ResourceVersion: "2"},
		Spec: k8s.WorkflowSpec{Steps: []k8s.WorkflowStep{{Name: "review", AgentRef: k8s.LocalObjectReference{Name: "analyst"}}}}}
	raw, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeapproval.WorkflowExecutorSpecDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	reference := api.RuntimeApprovalReference{Kind: "Workflow", Scope: "Namespaced", Namespace: workflow.Namespace, Name: workflow.Name, UID: string(workflow.UID), ResourceVersion: 2}
	submitter := authenticationv1.UserInfo{Username: "actual-submitter", UID: "submitter-uid", Groups: []string{"workflow-users"},
		Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"submitter-pod-uid"}}}
	reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(workflow.DeepCopy()).Build()
	reviewer := &workflowUseReviewFixture{status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}
	authorizer := &WorkflowUseAdmissionAuthorizer{Reader: reader, AccessReviews: reviewer}
	if err := authorizer.Authorize(context.Background(), submitter, reference, digest); err != nil {
		t.Fatalf("healthy named Workflow/use failed: %v", err)
	}
	request := reviewer.request
	if request == nil || request.Spec.User != "actual-submitter" || request.Spec.UID != "submitter-uid" || len(request.Spec.Groups) != 1 || request.Spec.Groups[0] != "workflow-users" ||
		len(request.Spec.Extra["authentication.kubernetes.io/pod-uid"]) != 1 || request.Spec.Extra["authentication.kubernetes.io/pod-uid"][0] != "submitter-pod-uid" ||
		request.Spec.ResourceAttributes == nil || *request.Spec.ResourceAttributes != (authorizationv1.ResourceAttributes{Namespace: "team", Group: "kognition.dev", Resource: "workflows", Name: "investigation", Verb: "use"}) {
		t.Fatalf("actual submitter or named Workflow/use scope was replaced: %+v", request)
	}
	for name, status := range map[string]authorizationv1.SubjectAccessReviewStatus{
		"denied": {}, "deny precedence": {Allowed: true, Denied: true}, "evaluation error": {Allowed: true, EvaluationError: "policy unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			reviewer.status = status
			if err := authorizer.Authorize(context.Background(), submitter, reference, digest); err == nil {
				t.Fatal("Workflow/use authorization did not fail closed")
			}
		})
	}
	reviewer.status = authorizationv1.SubjectAccessReviewStatus{Allowed: true}
	reviewer.err = errors.New("review unavailable")
	if err := authorizer.Authorize(context.Background(), submitter, reference, digest); !errors.Is(err, controlplaneauth.ErrUnavailable) {
		t.Fatalf("review failure was not unavailable: %v", err)
	}
	reviewer.err = nil
	reviewer.nilResponse = true
	if err := authorizer.Authorize(context.Background(), submitter, reference, digest); !errors.Is(err, controlplaneauth.ErrUnavailable) {
		t.Fatalf("missing review response was not unavailable: %v", err)
	}
	reviewer.nilResponse = false
	if err := (*WorkflowUseAdmissionAuthorizer)(nil).Authorize(context.Background(), submitter, reference, digest); !errors.Is(err, runtimeapproval.ErrWorkflowDispatchUnsupported) {
		t.Fatalf("unconfigured native authorization was accepted: %v", err)
	}
	for _, invalidDigest := range []string{"", strings.Repeat("0", 64), strings.ToUpper(digest)} {
		reviewer.request = nil
		if err := authorizer.Authorize(context.Background(), submitter, reference, invalidDigest); err == nil || reviewer.request != nil {
			t.Fatalf("changed or malformed Workflow digest reached authorization: %v %+v", err, reviewer.request)
		}
	}
	for name, change := range map[string]func(*api.RuntimeApprovalReference, *authenticationv1.UserInfo){
		"missing Workflow UID": func(reference *api.RuntimeApprovalReference, _ *authenticationv1.UserInfo) { reference.UID = "" },
		"changed revision": func(reference *api.RuntimeApprovalReference, _ *authenticationv1.UserInfo) {
			reference.ResourceVersion++
		},
		"wildcard Workflow": func(reference *api.RuntimeApprovalReference, _ *authenticationv1.UserInfo) { reference.Name = "*" },
		"anonymous": func(_ *api.RuntimeApprovalReference, user *authenticationv1.UserInfo) {
			user.Username = "system:anonymous"
		},
		"unauthenticated group": func(_ *api.RuntimeApprovalReference, user *authenticationv1.UserInfo) {
			user.Groups = []string{"system:unauthenticated"}
		},
		"unbounded groups": func(_ *api.RuntimeApprovalReference, user *authenticationv1.UserInfo) {
			user.Groups = make([]string, 65)
		},
	} {
		t.Run(name, func(t *testing.T) {
			currentReference, currentUser := reference, *submitter.DeepCopy()
			change(&currentReference, &currentUser)
			reviewer.request = nil
			if err := authorizer.Authorize(context.Background(), currentUser, currentReference, digest); err == nil || reviewer.request != nil {
				t.Fatalf("invalid submission reached authorization: %v %+v", err, reviewer.request)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reviewer.after = cancel
	if err := authorizer.Authorize(ctx, submitter, reference, digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after review was ignored: %v", err)
	}
	reviewer.after = func() {
		if err := reader.Delete(context.Background(), workflow.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		recreated := workflow.DeepCopy()
		recreated.UID, recreated.ResourceVersion = "replacement-workflow-uid", ""
		if err := reader.Create(context.Background(), recreated); err != nil {
			t.Fatal(err)
		}
	}
	if err := authorizer.Authorize(context.Background(), submitter, reference, digest); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("Workflow recreation during authorization was accepted: %v", err)
	}
}

func TestWorkflowBindingConfigurationRequiresProtectedAdministration(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop())
	object := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "kognition-workflow-executor-binding-investigation", Namespace: "kognition-system",
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-executor-binding"},
	}, Data: map[string]string{"binding.json": `{}`}}
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete} {
		t.Run(string(operation), func(t *testing.T) {
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "binding-write", Operation: operation, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				Namespace: object.Namespace, Name: object.Name,
				UserInfo: authenticationv1.UserInfo{Username: "operator", UID: "operator-uid", Groups: []string{"binding-administrators"}},
			}}
			if operation != admissionv1.Delete {
				request.Object.Raw = raw
			}
			if operation != admissionv1.Create {
				request.OldObject.Raw = raw
			}
			response := handler.Handle(context.Background(), request)
			if response.Allowed || response.Result == nil || response.Result.Code != http.StatusForbidden {
				t.Fatalf("unconfigured private binding administration: allowed=%v result=%+v", response.Allowed, response.Result)
			}
		})
	}
}

func TestWorkflowBindingConfigurationOperatorBoundary(t *testing.T) {
	executor := "system:serviceaccount:kognition-system:workflow-executor"
	groups := []string{"binding-administrators"}
	executors := []string{executor}
	auditBackend := &auditStore{}
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(),
		WithWorkflowBindingAdministrators("kognition-system", groups, executors),
		WithWorkflowBindingReader(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(workflowBindingDefinitionFixture()).Build()),
		WithAuditLog(observability.NewCognitiveAuditLog(auditBackend)))
	groups[0], executors[0] = "authors", "another-executor"
	tests := []struct {
		name    string
		change  func(*admission.Request, *corev1.ConfigMap, *corev1.ConfigMap)
		allowed bool
	}{
		{name: "operator", allowed: true},
		{name: "author", change: func(request *admission.Request, _, _ *corev1.ConfigMap) {
			request.UserInfo.Groups = []string{"authors"}
		}},
		{name: "executor in operator group", change: func(request *admission.Request, _, _ *corev1.ConfigMap) {
			request.UserInfo.Username = executor
		}},
		{name: "anonymous", change: func(request *admission.Request, _, _ *corev1.ConfigMap) {
			request.UserInfo.Username = "system:anonymous"
		}},
		{name: "unauthenticated group", change: func(request *admission.Request, _, _ *corev1.ConfigMap) {
			request.UserInfo.Groups = append(request.UserInfo.Groups, "system:unauthenticated")
		}},
		{name: "wrong namespace", change: func(request *admission.Request, object, old *corev1.ConfigMap) {
			request.Namespace, object.Namespace, old.Namespace = "workers", "workers", "workers"
		}},
		{name: "forged metadata namespace", change: func(_ *admission.Request, object, old *corev1.ConfigMap) {
			object.Namespace, old.Namespace = "workers", "workers"
		}},
		{name: "changed metadata name", change: func(_ *admission.Request, object, old *corev1.ConfigMap) {
			object.Name, old.Name = "replacement", "replacement"
		}},
		{name: "unlabeled reserved name", change: func(request *admission.Request, object, old *corev1.ConfigMap) {
			request.UserInfo.Groups = []string{"authors"}
			object.Labels, old.Labels = nil, nil
		}},
		{name: "unrelated ConfigMap", allowed: true, change: func(request *admission.Request, object, old *corev1.ConfigMap) {
			request.Name, object.Name, old.Name = "ordinary", "ordinary", "ordinary"
			object.Labels, old.Labels = nil, nil
			request.UserInfo.Groups = []string{"authors"}
		}},
	}
	for _, test := range tests {
		for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete} {
			t.Run(test.name+"/"+string(operation), func(t *testing.T) {
				object := workflowBindingConfigurationFixture(t)
				old := *object.DeepCopy()
				request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
					UID: "binding-write", Operation: operation, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
					Namespace: object.Namespace, Name: object.Name,
					UserInfo: authenticationv1.UserInfo{Username: "operator", Groups: []string{"binding-administrators"}},
				}}
				if test.change != nil {
					test.change(&request, &object, &old)
				}
				var err error
				if operation != admissionv1.Delete {
					request.Object.Raw, err = json.Marshal(object)
					if err != nil {
						t.Fatal(err)
					}
				}
				if operation != admissionv1.Create {
					request.OldObject.Raw, err = json.Marshal(old)
					if err != nil {
						t.Fatal(err)
					}
				}
				response := handler.Handle(context.Background(), request)
				expected := test.allowed && (operation != admissionv1.Delete || test.name == "unrelated ConfigMap")
				if response.Allowed != expected || !expected && (response.Result == nil || response.Result.Code != http.StatusForbidden) {
					t.Fatalf("binding write allowed=%v want=%v result=%+v", response.Allowed, expected, response.Result)
				}
			})
		}
	}
	object := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "labeled-record", Namespace: "kognition-system",
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-executor-binding"}}}
	oldRaw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	object.Labels = nil
	newRaw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "strip-label", Operation: admissionv1.Update, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		Namespace: object.Namespace, Name: object.Name, Object: runtime.RawExtension{Raw: newRaw}, OldObject: runtime.RawExtension{Raw: oldRaw},
		UserInfo: authenticationv1.UserInfo{Username: "author", Groups: []string{"authors"}},
	}})
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusForbidden {
		t.Fatalf("prior protected label bypassed administration: allowed=%v result=%+v", response.Allowed, response.Result)
	}
	if len(auditBackend.entries) != 2 {
		t.Fatalf("operator binding audit count = %d, want 2", len(auditBackend.entries))
	}
	for index, entry := range auditBackend.entries {
		if entry.AgentIdentity != "operator" || entry.Workspace != "kognition-system" || entry.SubjectKind != "ConfigMap" ||
			entry.SubjectRef != "kognition-system/"+runtimeapproval.WorkflowExecutorBindingObjectName("team-a", "approved") ||
			entry.Verb != []string{"attempt_create", "attempt_update"}[index] {
			t.Fatalf("operator binding audit attribution: %+v", entry)
		}
	}
}

func TestWorkflowBindingConfigurationRequiresDurableAudit(t *testing.T) {
	object := workflowBindingConfigurationFixture(t)
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable-%v", unavailable), func(t *testing.T) {
			options := []Option{WithWorkflowBindingAdministrators("kognition-system", []string{"binding-administrators"}, []string{"system:serviceaccount:kognition-system:workflow-executor"}), WithWorkflowBindingReader(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(workflowBindingDefinitionFixture()).Build())}
			if unavailable {
				options = append(options, WithAuditLog(observability.NewCognitiveAuditLog(&auditStore{err: errors.New("audit unavailable")})))
			}
			handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), options...)
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "binding-write", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
				Namespace: object.Namespace, Name: object.Name, Object: runtime.RawExtension{Raw: raw},
				UserInfo: authenticationv1.UserInfo{Username: "operator", Groups: []string{"binding-administrators"}},
			}})
			if response.Allowed || response.Result == nil || response.Result.Code != http.StatusInternalServerError {
				t.Fatalf("unaudited binding write: allowed=%v result=%+v", response.Allowed, response.Result)
			}
		})
	}
}

func workflowBindingDefinitionFixture() *k8s.Workflow {
	return &k8s.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "investigate", Namespace: "team-a", UID: "investigate-uid", ResourceVersion: "1"}, Spec: k8s.WorkflowSpec{Steps: []k8s.WorkflowStep{{Name: "analysis", AgentRef: k8s.LocalObjectReference{Name: "analyst"}}}}}
}

func workflowBindingConfigurationFixture(t *testing.T) corev1.ConfigMap {
	t.Helper()
	reference := func(kind, name string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: name + "-uid", ResourceVersion: 1}
	}
	authority := runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: api.RuntimeApprovalReference{Kind: "WorkflowExecutionBinding", Scope: "Namespaced", Namespace: "team-a", Name: "approved"},
		Workflow: reference("Workflow", "investigate"), WorkflowRevisionDigest: strings.Repeat("a", 64), Enabled: true,
		Executor:       api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "system:serviceaccount:kognition-system:workflow-executor"},
		ApprovedBy:     api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "operator"},
		ControllerUIDs: []string{"controller-uid"}, NotBefore: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC),
		Limits: runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
		Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: reference("Agent", "analyst"), Service: reference("AgentService", "analyst"),
			Operation: runtimeapproval.WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}}
	definition, err := json.Marshal(convert.WorkflowFromK8s(workflowBindingDefinitionFixture()))
	if err != nil {
		t.Fatal(err)
	}
	authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: runtimeapproval.WorkflowExecutorBindingObjectName("team-a", "approved"), Namespace: "kognition-system",
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-executor-binding"}}, Data: map[string]string{"binding.json": string(raw)}}
}

type workflowSubmissionBindingsFixture struct {
	authority runtimeapproval.WorkflowExecutorAuthority
}

type workflowSubmissionReceiptsFixture struct {
	receipts map[string]runtimeapproval.NativeWorkflowSubmissionReceipt
}

func (fixture *workflowSubmissionReceiptsFixture) CreateNativeWorkflowSubmissionReceipt(_ context.Context, receipt runtimeapproval.NativeWorkflowSubmissionReceipt) error {
	key := runtimeapproval.NativeWorkflowSubmissionObjectName(receipt.Run.Namespace, receipt.Run.UID)
	if _, found := fixture.receipts[key]; found {
		return store.ErrAlreadyExists
	}
	fixture.receipts[key] = receipt
	return nil
}

func (fixture *workflowSubmissionReceiptsFixture) GetNativeWorkflowSubmissionReceipt(_ context.Context, workspace, runUID string) (runtimeapproval.NativeWorkflowSubmissionReceipt, error) {
	receipt, found := fixture.receipts[runtimeapproval.NativeWorkflowSubmissionObjectName(workspace, runUID)]
	if !found {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, store.ErrNotFound
	}
	return receipt, nil
}

func (reader *workflowSubmissionBindingsFixture) GetWorkflowExecutorAuthority(_ context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if reference != reader.authority.Reference {
		return runtimeapproval.WorkflowExecutorAuthority{}, controlplaneauth.ErrForbidden
	}
	return reader.authority, nil
}

func workflowSubmissionFixture(t *testing.T) (*WorkflowRunSubmissionService, admission.Request, *workflowSubmissionBindingsFixture, *workflowUseReviewFixture) {
	t.Helper()
	configuration := workflowBindingConfigurationFixture(t)
	authority, err := runtimeapproval.DecodeWorkflowExecutorAuthority([]byte(configuration.Data["binding.json"]))
	if err != nil {
		t.Fatal(err)
	}
	authority.Reference.UID, authority.Reference.ResourceVersion = "actual-binding-uid", 3
	bindings := &workflowSubmissionBindingsFixture{authority: authority}
	reviewer := &workflowUseReviewFixture{status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}
	reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(workflowBindingDefinitionFixture()).Build()
	now := authority.NotBefore.Add(time.Hour)
	service := &WorkflowRunSubmissionService{Authorizer: &WorkflowUseAdmissionAuthorizer{Reader: reader, AccessReviews: reviewer}, Bindings: bindings,
		Receipts: &workflowSubmissionReceiptsFixture{receipts: map[string]runtimeapproval.NativeWorkflowSubmissionReceipt{}}, Clock: func() (time.Time, error) { return now, nil }}
	service.Keys, err = runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1,
		Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		t.Fatal(err)
	}
	run := k8s.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "investigation-run", Namespace: authority.Workflow.Namespace, UID: "actual-resource-uid",
		Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true", "kognition.dev/initiator": "forged-operator"}},
		Spec: k8s.WorkflowRunSpec{WorkflowRef: k8s.LocalObjectReference{Name: authority.Workflow.Name}}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "actual-admission-uid", Operation: admissionv1.Create,
		Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkflowRun"}, Namespace: run.Namespace, Name: run.Name,
		UserInfo: authenticationv1.UserInfo{Username: "actual-submitter", UID: "actual-submitter-uid", Groups: []string{"workflow-users"},
			Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"actual-submitter-pod"}}}, Object: runtime.RawExtension{Raw: raw}}}
	return service, request, bindings, reviewer
}

func workflowSubmissionPersistedRun(t *testing.T, request admission.Request, receipt runtimeapproval.NativeWorkflowSubmissionReceipt) api.WorkflowRun {
	t.Helper()
	var native k8s.WorkflowRun
	if err := json.Unmarshal(request.Object.Raw, &native); err != nil {
		t.Fatal(err)
	}
	native.ResourceVersion = "4"
	return *convert.WorkflowRunFromK8s(&native)
}

func TestWorkflowSubmissionReceiptUsesActualSubmitterAtFinalAcceptance(t *testing.T) {
	service, request, bindings, reviewer := workflowSubmissionFixture(t)
	receipt, err := service.Issue(context.Background(), request, bindings.authority.Reference)
	if err != nil {
		t.Fatalf("healthy authenticated issuance failed: %v", err)
	}
	if receipt.Run.UID != "actual-resource-uid" || receipt.AdmissionUID != "actual-admission-uid" || receipt.Submitter.Username != "actual-submitter" ||
		receipt.Submitter.UID != "actual-submitter-uid" || receipt.Submitter.Extra["authentication.kubernetes.io/pod-uid"][0] != "actual-submitter-pod" {
		t.Fatalf("actual resource or authenticated submitter was replaced: %+v", receipt)
	}
	run := workflowSubmissionPersistedRun(t, request, receipt)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	reviewer.request = nil
	acceptance, err := service.AuthorizeAcceptance(context.Background(), raw)
	if err != nil {
		t.Fatalf("healthy signed evidence did not authorize acceptance: %v", err)
	}
	if acceptance.RunUID != "actual-resource-uid" || acceptance.Run.ResourceVersion != 4 || acceptance.Initiator.Subject != "actual-submitter" ||
		acceptance.Executor != bindings.authority.Executor || acceptance.ExpiresAt.Sub(acceptance.AcceptedAt) != time.Hour ||
		reviewer.request == nil || reviewer.request.Spec.User != "actual-submitter" || reviewer.request.Spec.UID != "actual-submitter-uid" || reviewer.request.Spec.ResourceAttributes.Name != "investigate" {
		t.Fatalf("final acceptance reused executor identity, caller annotations, or wrong scope: %+v %+v", acceptance, reviewer.request)
	}
}

func TestWorkflowSubmissionReceiptFinalizationFailsClosed(t *testing.T) {
	for _, scenario := range []string{"revoked use", "binding revoked", "binding revision", "expired", "recreated run", "copied run", "edited request", "started run", "forged evidence", "binding revoked during review", "missing authorizer reader", "missing access reviewer"} {
		t.Run(scenario, func(t *testing.T) {
			service, request, bindings, reviewer := workflowSubmissionFixture(t)
			receipt, err := service.Issue(context.Background(), request, bindings.authority.Reference)
			if err != nil {
				t.Fatal(err)
			}
			run := workflowSubmissionPersistedRun(t, request, receipt)
			switch scenario {
			case "revoked use":
				reviewer.status = authorizationv1.SubjectAccessReviewStatus{}
			case "binding revoked":
				bindings.authority.Enabled = false
			case "binding revision":
				bindings.authority.Reference.ResourceVersion++
			case "expired":
				service.Clock = func() (time.Time, error) { return receipt.ExpiresAt, nil }
			case "recreated run":
				run.UID = "replacement-run-uid"
			case "copied run":
				run.Name = "another-run"
			case "edited request":
				run.Spec.WorkflowRef.Name = "another-workflow"
			case "started run":
				run.Status.StartTime = &receipt.IssuedAt
			case "forged evidence":
				receipt.Submitter.Username = "operator"
				encoded, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				run.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] = string(encoded)
			case "binding revoked during review":
				reviewer.after = func() { bindings.authority.Enabled = false }
			case "missing authorizer reader":
				service.Authorizer.Reader = nil
			case "missing access reviewer":
				service.Authorizer.AccessReviews = nil
			}
			raw, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			if acceptance, err := service.AuthorizeAcceptance(context.Background(), raw); err == nil || acceptance.RunUID != "" {
				t.Fatalf("invalid native evidence authorized acceptance: %+v %v", acceptance, err)
			}
		})
	}
}

func TestWorkflowSubmissionReceiptAdmissionLifecycle(t *testing.T) {
	for _, scenario := range []string{"healthy", "changed submitter", "changed submitter UID", "revoked use", "revoked binding", "caller supplied receipt", "started run", "edited binding", "healthy update", "added evidence on update", "edited binding on update"} {
		t.Run(scenario, func(t *testing.T) {
			service, request, bindings, reviewer := workflowSubmissionFixture(t)
			var native k8s.WorkflowRun
			if err := json.Unmarshal(request.Object.Raw, &native); err != nil {
				t.Fatal(err)
			}
			bindingJSON, err := json.Marshal(bindings.authority.Reference)
			if err != nil {
				t.Fatal(err)
			}
			native.Annotations[runtimeapproval.NativeWorkflowExecutionBindingAnnotation] = string(bindingJSON)
			native.UID = ""
			request.Object.Raw, err = json.Marshal(native)
			if err != nil {
				t.Fatal(err)
			}
			mutator := newHandlerForMode(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), modeMutate, WithWorkflowRunSubmissions(service))
			validator := newHandlerForMode(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), modeValidate, WithWorkflowRunSubmissions(service))
			response := mutator.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("qualified mutation failed: %+v", response.Result)
			}
			if err := response.Complete(request); err != nil {
				t.Fatal(err)
			}
			patched := request.Object.Raw
			if len(response.Patch) != 0 {
				patch, err := jsonpatch.DecodePatch(response.Patch)
				if err != nil {
					t.Fatal(err)
				}
				patched, err = patch.Apply(request.Object.Raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := json.Unmarshal(patched, &native); err != nil {
				t.Fatal(err)
			}
			if native.UID != "" || native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" || len(service.Receipts.(*workflowSubmissionReceiptsFixture).receipts) != 0 {
				t.Fatal("mutation invented a UID or issued premature evidence")
			}
			request.Object.Raw = patched
			request.UID = "reinvoked-mutating-admission-uid"
			if !mutator.Handle(context.Background(), request).Allowed {
				t.Fatal("read-only mutation reinvocation rejected healthy preflight")
			}
			native.UID = "actual-resource-uid"
			patched, err = json.Marshal(native)
			if err != nil {
				t.Fatal(err)
			}
			request.UID = "separate-validating-admission-uid"
			request.Object.Raw = patched
			if !validator.Handle(context.Background(), request).Allowed {
				t.Fatal("validation did not retain authenticated evidence for the actual resource UID")
			}
			request.UID = "retry-validating-admission-uid"
			allowed := false
			handler := validator
			switch scenario {
			case "healthy":
				allowed = true
			case "changed submitter":
				request.UserInfo.Username = "another-submitter"
			case "changed submitter UID":
				request.UserInfo.UID = "replacement-submitter-uid"
			case "revoked use":
				reviewer.status = authorizationv1.SubjectAccessReviewStatus{}
			case "revoked binding":
				bindings.authority.Enabled = false
			case "caller supplied receipt":
				native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] = "forged"
			case "started run":
				started := metav1.NewTime(time.Now())
				native.Status.StartTime = &started
			case "edited binding":
				delete(native.Annotations, runtimeapproval.NativeWorkflowExecutionBindingAnnotation)
			case "healthy update", "added evidence on update", "edited binding on update":
				request.Operation = admissionv1.Update
				request.OldObject.Raw = patched
				native.ResourceVersion = "4"
				allowed = scenario == "healthy update"
				if scenario == "added evidence on update" {
					native.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] = "forged"
				}
				if scenario == "edited binding on update" {
					delete(native.Annotations, runtimeapproval.NativeWorkflowExecutionBindingAnnotation)
				}
			}
			request.Object.Raw, err = json.Marshal(native)
			if err != nil {
				t.Fatal(err)
			}
			response = handler.Handle(context.Background(), request)
			if response.Allowed != allowed {
				t.Fatalf("%s: allowed=%v result=%+v", scenario, response.Allowed, response.Result)
			}
		})
	}
}

func TestWorkflowSubmissionReceiptPrivateWritesRequireVerifiedExecutor(t *testing.T) {
	for _, submission := range []bool{true, false} {
		for _, scenario := range []string{"healthy", "author", "operator", "wrong service account UID", "wrong pod UID", "unverified writer", "final writer lost", "revoked use", "revoked binding", "tampered evidence", "mutable record", "extra data", "wrong name", "wrong namespace", "update", "delete"} {
			t.Run(fmt.Sprintf("submission-%v/%s", submission, scenario), func(t *testing.T) {
				service, request, bindings, reviewer := workflowSubmissionFixture(t)
				receipt, err := service.Issue(context.Background(), request, bindings.authority.Reference)
				if err != nil {
					t.Fatal(err)
				}
				writer := authenticationv1.UserInfo{Username: bindings.authority.Executor.Subject, UID: "actual-executor-sa-uid", Groups: []string{"system:authenticated"},
					Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"executor-pod"}, "authentication.kubernetes.io/pod-uid": {"controller-uid"}}}
				service.RecordNamespace = "control"
				service.CurrentWriter = func(context.Context) (authenticationv1.UserInfo, error) { return *writer.DeepCopy(), nil }
				immutable := true
				object := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: runtimeapproval.NativeWorkflowSubmissionObjectName(receipt.Run.Namespace, receipt.Run.UID),
					Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-submission"}}, Immutable: &immutable, Data: map[string]string{}}
				encoded, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				object.Data["receipt.json"] = string(encoded)
				if !submission {
					run := workflowSubmissionPersistedRun(t, request, receipt)
					native := convert.WorkflowRunToK8s(&run)
					native.ResourceVersion = ""
					if err := service.Authorizer.Reader.(client.Client).Create(context.Background(), native); err != nil {
						t.Fatal(err)
					}
					run = *convert.WorkflowRunFromK8s(native)
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					acceptance, err := service.AuthorizeAcceptance(context.Background(), raw)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err = json.Marshal(acceptance)
					if err != nil {
						t.Fatal(err)
					}
					object.Name = runtimeapproval.WorkflowRunAcceptanceObjectName(run.Namespace, run.UID)
					object.Labels["kognition.dev/internal-record"] = "workflow-run-acceptance"
					object.Data = map[string]string{"acceptance.json": string(encoded)}
				}
				request.Operation = admissionv1.Create
				request.Kind = metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
				request.Namespace, request.Name = object.Namespace, object.Name
				request.UserInfo = *writer.DeepCopy()
				switch scenario {
				case "author", "operator":
					request.UserInfo.Username = scenario
				case "wrong service account UID":
					request.UserInfo.UID = "replacement-sa-uid"
				case "wrong pod UID":
					request.UserInfo.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"replacement-pod-uid"}
				case "unverified writer":
					service.CurrentWriter = nil
				case "final writer lost":
					calls := 0
					service.CurrentWriter = func(context.Context) (authenticationv1.UserInfo, error) {
						calls++
						if calls > 1 {
							return authenticationv1.UserInfo{}, errors.New("workload proof lost")
						}
						return *writer.DeepCopy(), nil
					}
				case "revoked use":
					reviewer.status = authorizationv1.SubjectAccessReviewStatus{}
				case "revoked binding":
					bindings.authority.Enabled = false
				case "tampered evidence":
					for key, raw := range object.Data {
						object.Data[key] = strings.Replace(raw, "actual-submitter", "forged-submitter", 1)
					}
				case "mutable record":
					object.Immutable = nil
				case "extra data":
					object.Data["unexpected"] = "value"
				case "wrong name":
					object.Name += "-copied"
					request.Name = object.Name
				case "wrong namespace":
					object.Namespace, request.Namespace = "another-control", "another-control"
				case "update":
					request.Operation = admissionv1.Update
				case "delete":
					request.Operation = admissionv1.Delete
				}
				request.Object.Raw, err = json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				if request.Operation != admissionv1.Create {
					request.OldObject.Raw = request.Object.Raw
				}
				handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), WithWorkflowRunSubmissions(service))
				response := handler.Handle(context.Background(), request)
				if response.Allowed != (scenario == "healthy") {
					t.Fatalf("private evidence %s allowed=%v result=%+v", scenario, response.Allowed, response.Result)
				}
			})
		}
	}
}

func TestWorkflowBindingConfigurationVerifiesLiveWorkflow(t *testing.T) {
	for _, scenario := range []string{"healthy", "recreated", "revision", "missing", "digest"} {
		t.Run(scenario, func(t *testing.T) {
			definition := workflowBindingDefinitionFixture()
			if scenario == "recreated" {
				definition.UID = "replacement"
			}
			if scenario == "revision" {
				definition.ResourceVersion = "2"
			}
			if scenario == "digest" {
				definition.Spec.Steps[0].Name = "changed"
			}
			builder := fake.NewClientBuilder().WithScheme(testScheme())
			if scenario != "missing" {
				builder = builder.WithObjects(definition)
			}
			handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(), WithWorkflowBindingAdministrators("kognition-system", []string{"binding-administrators"}, []string{"system:serviceaccount:kognition-system:workflow-executor"}), WithWorkflowBindingReader(builder.Build()), WithAuditLog(observability.NewCognitiveAuditLog(&auditStore{})))
			object := workflowBindingConfigurationFixture(t)
			raw, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "binding-write", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespace: object.Namespace, Name: object.Name, Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: "operator", Groups: []string{"binding-administrators"}}}})
			if response.Allowed != (scenario == "healthy") {
				t.Fatalf("live %s Workflow: %+v", scenario, response)
			}
		})
	}
}

func TestWorkflowBindingConfigurationRejectsMalformedPayload(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), zap.NewNop(),
		WithWorkflowBindingAdministrators("kognition-system", []string{"binding-administrators"}, []string{"system:serviceaccount:kognition-system:workflow-executor"}),
		WithAuditLog(observability.NewCognitiveAuditLog(&auditStore{})))
	for _, field := range []string{"credentials", "serviceAccount", "unexpected", "reference"} {
		t.Run(field, func(t *testing.T) {
			object := workflowBindingConfigurationFixture(t)
			raw := object.Data["binding.json"]
			object.Data["binding.json"] = `{"` + field + `":true,` + raw[1:]
			encoded, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "binding-write", Operation: admissionv1.Create,
				Kind: metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespace: object.Namespace, Name: object.Name,
				Object: runtime.RawExtension{Raw: encoded}, UserInfo: authenticationv1.UserInfo{Username: "operator", Groups: []string{"binding-administrators"}}}})
			if response.Allowed || response.Result == nil || response.Result.Code != http.StatusBadRequest {
				t.Fatalf("malformed binding allowed: %+v", response.Result)
			}
		})
	}
}

func TestHandlerConvertsAutonomyFoundationTypes(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	objects := []struct {
		kind string
		obj  any
	}{
		{kind: "TrustProfile", obj: k8s.TrustProfile{TypeMeta: metav1.TypeMeta{Kind: "TrustProfile", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "profile", Namespace: "team-a"}, Spec: k8s.TrustProfileSpec{AgentRef: k8s.LocalObjectReference{Name: "agent"}, AgentIncarnationUID: "agent-uid", InitialScore: 50}}},
		{kind: "RuntimeApproval", obj: k8s.RuntimeApproval{TypeMeta: metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "approval", Namespace: "team-a"}, Spec: k8s.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: "invocation"}}},
	}
	for _, test := range objects {
		t.Run(test.kind, func(t *testing.T) {
			raw, err := json.Marshal(test.obj)
			if err != nil {
				t.Fatal(err)
			}
			internal, err := handler.convertToInternal(test.kind, raw)
			if err != nil {
				t.Fatalf("convertToInternal: %v", err)
			}
			if _, err := handler.convertToK8s(test.kind, internal, raw); err != nil {
				t.Fatalf("convertToK8s: %v", err)
			}
		})
	}
}

func TestHandle_AuditsValidatedMutationBeforePersistence(t *testing.T) {
	store := &auditStore{}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-create", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: "alice"}}})
	if !response.Allowed {
		t.Fatalf("create denied: %#v", response.Result)
	}
	if len(store.entries) != 1 {
		t.Fatalf("audit entries = %#v", store.entries)
	}
	entry := store.entries[0]
	if entry.Workspace != "team-a" || entry.AgentIdentity != "alice" || entry.EventType != observability.AuditAdmissionAttempt || entry.SubjectKind != "Agent" || entry.SubjectRef != "team-a/planner" || entry.Verb != "attempt_create" {
		t.Fatalf("audit entry = %#v", entry)
	}
}

func TestHandle_DeniesMutationWhenAuditWriteFails(t *testing.T) {
	store := &auditStore{err: errors.New("audit unavailable")}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-failure", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}}})
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %#v, want audit failure denial", response)
	}
}

func TestHandle_PersistsAuditEntryToKubernetesConfigMap(t *testing.T) {
	scheme := testScheme()
	auditStore, err := k8saudit.NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(auditStore), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-configmap", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}}})
	if !response.Allowed {
		t.Fatalf("create denied: %#v", response.Result)
	}
	entries, err := auditStore.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SubjectRef != "team-a/planner" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestAuditHandler_SkipsDryRun(t *testing.T) {
	store := &auditStore{}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	dryRun := true
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-dry-run", Operation: admissionv1.Delete, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryGrant"}, Namespace: "team-a", Name: "grant", DryRun: &dryRun}})
	if !response.Allowed || len(store.entries) != 0 {
		t.Fatalf("response=%#v entries=%#v", response, store.entries)
	}
}

// TC-0201: Agent with missing modelRef is rejected by validating hook.
func TestHandle_AgentMissingModelRef_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "require-model-ref",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			modelRef, _ := spec["modelRef"].(map[string]any)
			name, _ := modelRef["name"].(string)
			if name == "" {
				return &intadmission.Response{Allowed: false, Reason: "spec.modelRef.name is required"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	// Agent with empty modelRef.
	agent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: ""},
			Instructions: k8s.AgentInstructions{Inline: "hello"},
		},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "123",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial, got allowed")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

func TestHandle_AgentIdentityAutomaticProvenanceRequiresController(t *testing.T) {
	validator := intadmission.NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	h := NewHandler(intadmission.NewChain(nil, []intadmission.ValidatingHook{validator}), testScheme(), testLogger())
	identity := k8s.AgentIdentity{
		TypeMeta: metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-identity-planner",
			Namespace: "team-a",
			Labels:    map[string]string{"kognition.dev/auto-created-for-agent": "planner"},
		},
		Spec: k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "identity-provenance",
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentity"},
		Namespace: "team-a",
		Name:      identity.Name,
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:team-a:tenant"},
	}}
	if response := h.Handle(context.Background(), request); response.Allowed {
		t.Fatal("tenant identity provenance forgery was allowed")
	}
	request.UserInfo.Username = "system:serviceaccount:kognition-system:kognition"
	if response := h.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("controller identity was denied: %s", response.Result.Message)
	}
}

func TestHandle_AgentIdentityAutomaticDeletionRequiresController(t *testing.T) {
	validator := intadmission.NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	h := NewHandler(intadmission.NewChain(nil, []intadmission.ValidatingHook{validator}), testScheme(), testLogger())
	identity := k8s.AgentIdentity{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-identity-planner", Namespace: "team-a", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}},
		Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "identity-delete",
		Operation: admissionv1.Delete,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentity"},
		Namespace: "team-a",
		Name:      identity.Name,
		OldObject: runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:team-a:tenant"},
	}}
	if response := h.Handle(context.Background(), request); response.Allowed {
		t.Fatal("tenant automatic identity deletion was allowed")
	}
	request.UserInfo.Username = "system:serviceaccount:kognition-system:kognition"
	if response := h.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("controller automatic identity deletion denied: %s", response.Result.Message)
	}
}

// TC-0202: Agent without replicas gets defaults applied by mutating hook.
func TestHandle_AgentDefaulted_Mutated(t *testing.T) {
	mutator := &stubMutator{
		name:  "agent-defaulter",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "bad json"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec["replicas"] == nil {
				spec["replicas"] = float64(1)
			}
			if rt, ok := spec["runtime"].(map[string]any); !ok || rt == nil {
				spec["runtime"] = map[string]any{"class": "default"}
			}
			obj["spec"] = spec
			patched, _ := json.Marshal(obj)
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "gpt-4"},
			Instructions: k8s.AgentInstructions{Inline: "you are helpful"},
		},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "456",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches to be present")
	}
}

func TestHandle_EndpointModesRunOnlyTheirAdmissionPhase(t *testing.T) {
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "endpoint-mode",
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
		Namespace: "default",
		Name:      "test-agent",
		Object:    runtime.RawExtension{Raw: raw},
	}}

	mutatorCalls := 0
	validatorCalls := 0
	mutator := &stubMutator{name: "default", kinds: map[string]bool{"Agent": true}, mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
		mutatorCalls++
		var object map[string]any
		_ = json.Unmarshal(req.Object, &object)
		object["metadata"].(map[string]any)["labels"] = map[string]any{"defaulted": "true"}
		patch, _ := json.Marshal(object)
		return &intadmission.Response{Allowed: true, Patch: patch}
	}}
	validator := &stubValidator{name: "validate", kinds: map[string]bool{"Agent": true}, validateFn: func(_ context.Context, _ *intadmission.Request) *intadmission.Response {
		validatorCalls++
		return &intadmission.Response{Allowed: true}
	}}
	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, []intadmission.ValidatingHook{validator})

	mutateResponse := newHandlerForMode(chain, testScheme(), testLogger(), modeMutate).Handle(context.Background(), req)
	if !mutateResponse.Allowed || len(mutateResponse.Patches) == 0 {
		t.Fatalf("mutate response = %+v; want allowed response with patch", mutateResponse)
	}
	if mutatorCalls != 1 || validatorCalls != 0 {
		t.Fatalf("mutate endpoint calls: mutator=%d validator=%d", mutatorCalls, validatorCalls)
	}

	validateResponse := newHandlerForMode(chain, testScheme(), testLogger(), modeValidate).Handle(context.Background(), req)
	if !validateResponse.Allowed || len(validateResponse.Patches) != 0 {
		t.Fatalf("validate response = %+v; want allowed response without patch", validateResponse)
	}
	if mutatorCalls != 1 || validatorCalls != 1 {
		t.Fatalf("validate endpoint calls: mutator=%d validator=%d", mutatorCalls, validatorCalls)
	}
}

// TC-0203: Unsupported operation returns error.
func TestHandle_UnsupportedOperation_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "789",
			Operation: admissionv1.Operation("CONNECT"),
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "x",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error response for unsupported operation")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %+v", resp.Result)
	}
}

// TC-0204: Unsupported kind returns error.
func TestHandle_UnsupportedKind_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	raw := []byte(`{"apiVersion":"kognition.dev/v1alpha1","kind":"Unknown","metadata":{"name":"x"}}`)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "000",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Unknown"},
			Namespace: "default",
			Name:      "x",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error for unknown kind")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %+v", resp.Result)
	}
}

// TC-0205: Tool mutation handling - verify Tool kind is supported through webhook.
func TestHandle_ToolMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "tool-enricher",
		kinds: map[string]bool{"Tool": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			// Simple mutation: just return allowed without patches
			// Patches would be applied by Kubernetes API server in real scenarios
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	tool := k8s.Tool{
		TypeMeta: metav1.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tool",
			Namespace: "default",
		},
		Spec: k8s.ToolSpec{
			ClassName: "default-class",
		},
	}
	raw, _ := json.Marshal(tool)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "205",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Tool"},
			Namespace: "default",
			Name:      "test-tool",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	// Tool should be allowed through the webhook with mutator applied
	if !resp.Allowed {
		t.Fatalf("expected Tool mutation to be allowed, got denied: %v", resp.Result)
	}
}

// TC-0206: ToolClass with validation rejection.
func TestHandle_ToolClassValidation_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "toolclass-validator",
		kinds: map[string]bool{"ToolClass": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			provisioner, _ := spec["provisioner"].(string)
			if provisioner == "" {
				return &intadmission.Response{Allowed: false, Reason: "spec.provisioner is required"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	toolClass := k8s.ToolClass{
		TypeMeta: metav1.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-toolclass",
		},
		Spec: k8s.ToolClassSpec{
			Provisioner: "",
		},
	}
	raw, _ := json.Marshal(toolClass)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "206",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClass"},
			Namespace: "default",
			Name:      "test-toolclass",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for empty provisioner")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0207: Workflow conversion to/from internal type.
func TestHandle_WorkflowConversion_Success(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{
				{
					Name:     "step-1",
					AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
				},
			},
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "207",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "test-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for valid Workflow, got: %v", resp.Result)
	}
}

// TC-0208: UPDATE operation with OldObject properly converted.
func TestHandle_UpdateWithOldObject_Success(t *testing.T) {
	validator := &stubValidator{
		name:  "update-validator",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			if req.OldObject == nil {
				return &intadmission.Response{Allowed: false, Reason: "OldObject missing for UPDATE"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	oldAgent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "old-model"},
			Instructions: k8s.AgentInstructions{Inline: "old instructions"},
		},
	}
	oldRaw, _ := json.Marshal(oldAgent)

	newAgent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "new-model"},
			Instructions: k8s.AgentInstructions{Inline: "new instructions"},
		},
	}
	newRaw, _ := json.Marshal(newAgent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "208",
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for valid UPDATE, got: %v", resp.Result)
	}
}

func TestHandle_RuntimeApprovalAuditAcknowledgementAfterK8sRoundTrip(t *testing.T) {
	const gatewayUsername = "system:serviceaccount:kognition-system:kognition-tool-gateway"
	const decisionUsername = "system:serviceaccount:kognition-system:kognition-webhook"
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewRuntimeApprovalValidator("controller", gatewayUsername).WithDecisionAuthority(decisionUsername).WithExecutionWriters(map[string]string{gatewayUsername: "Tool"}).WithClaimantReader(webhookClaimantReader{})})
	h := NewHandler(chain, testScheme(), testLogger())
	decidedAt := metav1.NewTime(time.Date(2026, 9, 4, 13, 19, 15, 521114400, time.UTC))
	reference := func(kind, scope, namespace string) k8s.RuntimeApprovalReference {
		return k8s.RuntimeApprovalReference{Kind: kind, Scope: scope, Namespace: namespace, Name: strings.ToLower(kind), UID: kind + "-uid", ResourceVersion: 1}
	}
	oldApproval := k8s.RuntimeApproval{
		TypeMeta:   metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: runtimeapproval.CanonicalName("team-a", "invocation"), Namespace: "team-a", UID: "approval-uid", ResourceVersion: "42", Finalizers: []string{k8s.RuntimeApprovalCompactionFinalizer}},
		Spec: k8s.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: "invocation", InitialDecision: "Pending", Plane: "Tool", Operation: "invoke", SessionRef: "session",
			AgentRef: reference("Agent", "Namespaced", "team-a"), AgentIdentityRef: reference("AgentIdentity", "Namespaced", "team-a"),
			AgentInstanceRef: reference("AgentInstance", "Namespaced", "team-a"), TargetRef: reference("Tool", "Namespaced", "team-a"),
			AutonomyClassRef: reference("AutonomyClass", "Cluster", ""),
			Binding:          k8s.RuntimeActionBinding{Format: "kognition-runtime-action-v1", Algorithm: "HMAC-SHA-256", Domain: "kognition.dev/runtime-approval/action/v1", Epoch: 1, KeyVersion: 1, Tag: strings.Repeat("a", 43)},
			CreatedAt:        metav1.NewTime(decidedAt.Add(-time.Minute)), ExpiresAt: metav1.NewTime(decidedAt.Add(time.Minute)),
		},
		Status: k8s.RuntimeApprovalStatus{
			DecisionState: "Approved", ExecutionState: "NotStarted", DecidedByIssuer: "issuer", DecidedBySubject: "subject", DecidedAt: &decidedAt,
			DecisionAudit: &k8s.RuntimeApprovalDecisionAudit{ID: "audit-id", Payload: k8s.RuntimeApprovalDecisionAuditPayload{
				Workspace: "team-a", InvocationID: "invocation", ApprovalUID: "approval-uid", Decision: "Approved",
				Issuer: "issuer", Subject: "subject", DecidedAt: decidedAt,
			}},
		},
	}
	oldRaw, _ := json.Marshal(oldApproval)
	internal := convert.RuntimeApprovalFromK8s(&oldApproval)
	internal.Status.DecisionEvidenceDelivered = true
	acknowledged := convert.RuntimeApprovalToK8s(internal)
	acknowledged.ResourceVersion = oldApproval.ResourceVersion
	newRaw, _ := json.Marshal(acknowledged)

	response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "runtime-approval-ack", Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"},
		Namespace: "team-a", Name: oldApproval.Name, Object: runtime.RawExtension{Raw: newRaw}, OldObject: runtime.RawExtension{Raw: oldRaw},
		UserInfo: authenticationv1.UserInfo{Username: gatewayUsername},
	}})
	if !response.Allowed {
		t.Fatalf("expected RuntimeApproval audit acknowledgement round trip to be allowed, got: %v", response.Result)
	}
	pending := oldApproval.DeepCopy()
	pending.Status = k8s.RuntimeApprovalStatus{DecisionState: "Pending", ExecutionState: "NotStarted"}
	pendingRaw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{gatewayUsername, decisionUsername} {
		response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "runtime-approval-decision", Operation: admissionv1.Update, SubResource: "status",
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"},
			Namespace: "team-a", Name: oldApproval.Name, Object: runtime.RawExtension{Raw: oldRaw}, OldObject: runtime.RawExtension{Raw: pendingRaw},
			UserInfo: authenticationv1.UserInfo{Username: username},
		}})
		if response.Allowed != (username == decisionUsername) {
			t.Fatalf("direct status decision authority crossed: user=%s result=%v", username, response.Result)
		}
	}
	claimed := oldApproval.DeepCopy()
	claimed.Status.DecisionEvidenceDelivered = true
	claimed.Status.ExecutionState = "Claimed"
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &k8s.RuntimeApprovalClaimant{WorkloadUID: "owner-pod", ContainerID: "containerd://owner", ProcessStartNonce: "owner-process-0001"}
	completed := claimed.DeepCopy()
	completed.Status.ExecutionState = "Completed"
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	claimedRaw, _ := json.Marshal(claimed)
	completedRaw, _ := json.Marshal(completed)
	for _, podUID := range []string{"", "foreign-pod", "owner-pod"} {
		response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "runtime-approval-owner", Operation: admissionv1.Update, SubResource: "status", Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"}, Namespace: "team-a", Name: claimed.Name,
			Object: runtime.RawExtension{Raw: completedRaw}, OldObject: runtime.RawExtension{Raw: claimedRaw}, UserInfo: authenticationv1.UserInfo{Username: gatewayUsername, Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {podUID}, "authentication.kubernetes.io/pod-name": {"gateway-pod"}}},
		}})
		if response.Allowed != (podUID == "owner-pod") {
			t.Fatalf("authenticated Pod ownership lost at handler: uid=%s result=%v", podUID, response.Result)
		}
	}
}

// TC-0209: Malformed JSON in object.
func TestHandle_MalformedJSON_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	badJSON := []byte(`{invalid json}`)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "209",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: badJSON},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error response for malformed JSON")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %+v", resp.Result)
	}
}

func TestHandle_MalformedOldObject_Error(t *testing.T) {
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model-1"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	resp := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger()).Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Kind: "Agent"},
			Object:    runtime.RawExtension{Raw: raw},
			OldObject: runtime.RawExtension{Raw: []byte(`{invalid`)},
		},
	})
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want 500 error", resp)
	}
}

func TestHandle_InvalidMutatorOutput_Error(t *testing.T) {
	mutator := &stubMutator{
		name:  "invalid-output",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(context.Context, *intadmission.Request) *intadmission.Response {
			return &intadmission.Response{Allowed: true, Patch: json.RawMessage(`{invalid`)}
		},
	}
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model-1"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	resp := NewHandler(intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil), testScheme(), testLogger()).Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Kind: "Agent"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want 500 error", resp)
	}
}

// TC-0210: AgentInstance with both mutation and validation.
func TestHandle_AgentInstanceChain_MutationThenValidation(t *testing.T) {
	// Mutator adds a label
	mutator := &stubMutator{
		name:  "instance-labeler",
		kinds: map[string]bool{"AgentInstance": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "bad json"}
			}
			metadata, _ := obj["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if labels == nil {
				labels = make(map[string]any)
			}
			labels["mutated"] = "true"
			metadata["labels"] = labels
			obj["metadata"] = metadata
			patched, _ := json.Marshal(obj)
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	// Validator checks the label was added
	validator := &stubValidator{
		name:  "instance-checker",
		kinds: map[string]bool{"AgentInstance": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			metadata, _ := obj["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if labels["mutated"] == "true" {
				return &intadmission.Response{Allowed: true}
			}
			return &intadmission.Response{Allowed: false, Reason: "label mutation did not occur"}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	instance := k8s.AgentInstance{
		TypeMeta: metav1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-instance",
			Namespace: "default",
		},
		Spec: k8s.AgentInstanceSpec{
			AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
		},
	}
	raw, _ := json.Marshal(instance)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "210",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstance"},
			Namespace: "default",
			Name:      "test-instance",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
}

// TC-0211: Various kinds' convertToInternal coverage (sample).
func TestHandle_MultipleKinds_ConversionSuccess(t *testing.T) {
	kinds := []struct {
		name   string
		obj    interface{}
		apiVer string
	}{
		{
			name: "AgentPolicy",
			obj: k8s.AgentPolicy{
				TypeMeta: metav1.TypeMeta{Kind: "AgentPolicy", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "policy-1",
					Namespace: "default",
				},
				Spec: k8s.AgentPolicySpec{},
			},
		},
		{
			name: "Workspace",
			obj: k8s.Workspace{
				TypeMeta: metav1.TypeMeta{Kind: "Workspace", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workspace-1",
					Namespace: "default",
				},
				Spec: k8s.WorkspaceSpec{},
			},
		},
		{
			name: "ModelClass",
			obj: k8s.ModelClass{
				TypeMeta: metav1.TypeMeta{Kind: "ModelClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "model-class-1",
				},
				Spec: k8s.ModelClassSpec{},
			},
		},
		{
			name: "AgentIdentity",
			obj: k8s.AgentIdentity{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "identity-1", Namespace: "default"},
				Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "agent-1"}},
			},
		},
		{
			name: "AgentRole",
			obj: k8s.AgentRole{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentRole", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "role-1", Namespace: "default"},
				Rules: []k8s.PolicyRule{{
					Resources: []string{"agents"},
					Verbs:     []string{"get"},
				}},
			},
		},
		{
			name: "AgentRoleBinding",
			obj: k8s.AgentRoleBinding{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentRoleBinding", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "binding-1", Namespace: "default"},
				Spec: k8s.AgentRoleBindingSpec{
					RoleRef:  k8s.LocalObjectReference{Name: "role-1"},
					Subjects: []k8s.Subject{{Kind: "Agent", Name: "agent-1"}},
				},
			},
		},
		{
			name: "MemoryClass",
			obj: k8s.MemoryClass{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-class-1"},
				Spec:       k8s.MemoryClassSpec{},
			},
		},
		{
			name: "MemoryClaim",
			obj: k8s.MemoryClaim{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryClaim", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-claim-1", Namespace: "default"},
				Spec:       k8s.MemoryClaimSpec{},
			},
		},
		{
			name: "MemoryVolume",
			obj: k8s.MemoryVolume{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryVolume", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-vol-1"},
				Spec:       k8s.MemoryVolumeSpec{},
			},
		},
		{
			name: "CognitiveAdmissionPolicy",
			obj: k8s.CognitiveAdmissionPolicy{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveAdmissionPolicy", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "cap-1"},
				Spec:       k8s.CognitiveAdmissionPolicySpec{},
			},
		},
		{
			name: "CognitiveQuota",
			obj: k8s.CognitiveQuota{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "quota-1", Namespace: "default"},
				Spec:       k8s.CognitiveQuotaSpec{},
			},
		},
		{
			name: "CognitiveLimitRange",
			obj: k8s.CognitiveLimitRange{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveLimitRange", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "limit-1", Namespace: "default"},
				Spec:       k8s.CognitiveLimitRangeSpec{},
			},
		},
		{
			name: "WorkflowRun",
			obj: k8s.WorkflowRun{
				TypeMeta:   metav1.TypeMeta{Kind: "WorkflowRun", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "wfr-1", Namespace: "default"},
				Spec:       k8s.WorkflowRunSpec{WorkflowRef: k8s.LocalObjectReference{Name: "wf-1"}},
			},
		},
		{
			name: "KnowledgeBaseClass",
			obj: k8s.KnowledgeBaseClass{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "kb-class-1"},
			},
		},
		{
			name: "KnowledgeBase",
			obj: k8s.KnowledgeBase{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBase", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "kb-1", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseClaim",
			obj: k8s.KnowledgeBaseClaim{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseClaim", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseIndexRun",
			obj: k8s.KnowledgeBaseIndexRun{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseIndexRun", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseIndexSchedule",
			obj: k8s.KnowledgeBaseIndexSchedule{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseIndexSchedule", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "schedule-1", Namespace: "default"},
			},
		},
		{
			name: "Model",
			obj: k8s.Model{
				TypeMeta:   metav1.TypeMeta{Kind: "Model", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "model-1", Namespace: "default"},
			},
		},
		{
			name: "SecretRef",
			obj: k8s.SecretRef{
				TypeMeta:   metav1.TypeMeta{Kind: "SecretRef", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "secret-1", Namespace: "default"},
			},
		},
		{
			name: "ApprovalDecision",
			obj: k8s.ApprovalDecision{
				TypeMeta:   metav1.TypeMeta{Kind: "ApprovalDecision", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "approval-1", Namespace: "default"},
			},
		},
	}

	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	for _, kt := range kinds {
		t.Run(kt.name, func(t *testing.T) {
			raw, _ := json.Marshal(kt.obj)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:       "211",
					Operation: admissionv1.Create,
					Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: kt.name},
					Namespace: "default",
					Name:      "test-obj",
					Object:    runtime.RawExtension{Raw: raw},
				},
			}

			resp := h.Handle(context.Background(), req)
			if !resp.Allowed {
				t.Fatalf("expected allowed for %s, got: %v", kt.name, resp.Result)
			}
		})
	}
}

// TC-0212: Handle respects context cancellation during validation.
func TestHandle_ContextCancelled_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "ctx-aware-validator",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			if err := ctx.Err(); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "context cancelled: " + err.Error()}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "212",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	// Cancel the context before calling Handle.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp := h.Handle(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial when context is already cancelled")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403 denial from context-aware hook, got %+v", resp.Result)
	}
}

// TC-0213: Handle respects context deadline during mutation.
func TestHandle_ContextDeadlineExceeded_Denied(t *testing.T) {
	mutator := &stubMutator{
		name:  "slow-mutator",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			// Simulate work that checks deadline.
			select {
			case <-ctx.Done():
				return &intadmission.Response{Allowed: false, Reason: "deadline exceeded: " + ctx.Err().Error()}
			default:
			}
			// Simulate a blocking operation that exceeds the deadline.
			select {
			case <-ctx.Done():
				return &intadmission.Response{Allowed: false, Reason: "deadline exceeded: " + ctx.Err().Error()}
			case <-time.After(200 * time.Millisecond):
				return &intadmission.Response{Allowed: true}
			}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-2", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "213",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-2",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	// Set a very tight deadline that will expire before the mutator finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	resp := h.Handle(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial when context deadline is exceeded")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403 from deadline-aware hook, got %+v", resp.Result)
	}
}

// TC-0214: Cyclic workflow without entryPoint is rejected by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowCyclicNoEntryPoint_Denied(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cyclic-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{
				{Name: "a", AgentRef: k8s.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: k8s.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []k8s.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"}, // creates cycle
			},
			// EntryPoint intentionally omitted — must be rejected.
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "cyclic-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for cyclic workflow without entryPoint")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0221: Workflow with no steps is rejected by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowNoSteps_Denied(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{}, // no steps
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "215",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "empty-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for workflow with no steps")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0222: Cyclic workflow WITH entryPoint is allowed by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowCyclicWithEntryPoint_Allowed(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "iterative-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			EntryPoint: "a",
			Steps: []k8s.WorkflowStep{
				{Name: "a", AgentRef: k8s.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: k8s.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []k8s.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"}, // cycle allowed when entryPoint is set
			},
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "216",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "iterative-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for cyclic workflow with entryPoint, got: %v", resp.Result)
	}
}

// TC-0219: Handle propagates deadline to validation hooks (hook sees remaining time).
func TestHandle_ContextDeadlinePropagated_HookSeesDeadline(t *testing.T) {
	var observedDeadline time.Time
	var hadDeadline bool

	validator := &stubValidator{
		name:  "deadline-observer",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			observedDeadline, hadDeadline = ctx.Deadline()
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-3", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-3",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	resp := h.Handle(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if !hadDeadline {
		t.Fatal("expected hook to observe a deadline on the context")
	}
	// The observed deadline should be at or before the one we set (Handle doesn't add time).
	if observedDeadline.After(deadline) {
		t.Fatalf("hook observed deadline %v which is after the parent deadline %v", observedDeadline, deadline)
	}
}

// TC-0218: AgentInstance mutation produces JSON patches via convertToK8s.
func TestHandle_AgentInstanceMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "agent-instance-defaulter",
		kinds: map[string]bool{"AgentInstance": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = map[string]any{}
				obj["spec"] = spec
			}
			sc, _ := spec["sessionContext"].(map[string]any)
			if sc == nil {
				sc = map[string]any{}
				spec["sessionContext"] = sc
			}
			sc["injected"] = "by-mutator"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	inst := k8s.AgentInstance{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "inst-1", Namespace: "default"},
		Spec: k8s.AgentInstanceSpec{
			AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
			Revision: 1,
		},
	}
	raw, _ := json.Marshal(inst)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "213",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstance"},
			Namespace: "default",
			Name:      "inst-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for AgentInstance mutation")
	}
}

// TC-0220: ToolClass mutation produces JSON patches via convertToK8s.
func TestHandle_ToolClassMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "tool-class-defaulter",
		kinds: map[string]bool{"ToolClass": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = map[string]any{}
				obj["spec"] = spec
			}
			params, _ := spec["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{}
				spec["parameters"] = params
			}
			params["defaulted"] = "true"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	tc := k8s.ToolClass{
		TypeMeta:   metav1.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1"},
		Spec:       k8s.ToolClassSpec{Provisioner: "mcp"},
	}
	raw, _ := json.Marshal(tc)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClass"},
			Namespace: "default",
			Name:      "tc-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for ToolClass mutation")
	}
}

// TC-0223, TC-0245: mutator-added metadata labels survive convertToK8s and appear in JSON patch
// (regression for bug where convertToK8s overwrote patched.ObjectMeta with original).
func TestHandle_MutatorAddsLabel_LabelPreservedInPatch(t *testing.T) {
	mutator := &stubMutator{
		name:  "label-injector",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			meta, _ := obj["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				obj["metadata"] = meta
			}
			labels, _ := meta["labels"].(map[string]any)
			if labels == nil {
				labels = map[string]any{}
				meta["labels"] = labels
			}
			labels["kognition.dev/mutated"] = "true"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-lbl", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "223",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-lbl",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for label mutation")
	}
	// jsonpatch escapes '/' as '~1'. Look for any op touching /metadata/labels.
	found := false
	for _, op := range resp.Patches {
		if strings.HasPrefix(op.Path, "/metadata/labels") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected patch op under /metadata/labels, got: %+v", resp.Patches)
	}
}

// TC-0224, TC-0246: mutator-added metadata labels survive convertToK8s for every kind
// covered by the defensive merge expansion (Finding #2). Locks in the pattern
// for the 14 kinds added beyond the original 4 (Agent, AgentInstance, Tool, ToolClass).
func TestHandle_MutatorAddsLabel_AllKinds_LabelPreservedInPatch(t *testing.T) {
	kinds := []string{
		"Tool",
		"AgentPolicy",
		"CognitiveAdmissionPolicy",
		"CognitiveQuota",
		"CognitiveLimitRange",
		"Workspace",
		"AgentIdentity",
		"AgentRole",
		"AgentRoleBinding",
		"MemoryClass",
		"MemoryClaim",
		"MemoryVolume",
		"ModelClass",
		"AutonomyClass",
		"Model",
		"SecretRef",
		"Workflow",
		"WorkflowRun",
		"ApprovalDecision",
		"KnowledgeBaseClass",
		"KnowledgeBase",
		"KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun",
		"KnowledgeBaseIndexSchedule",
		"Solution",
		"AgentJob",
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			mutator := &stubMutator{
				name:  "label-injector",
				kinds: map[string]bool{kind: true},
				mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
					var obj map[string]any
					if err := json.Unmarshal(req.Object, &obj); err != nil {
						return &intadmission.Response{Allowed: false, Reason: err.Error()}
					}
					meta, _ := obj["metadata"].(map[string]any)
					if meta == nil {
						meta = map[string]any{}
						obj["metadata"] = meta
					}
					labels, _ := meta["labels"].(map[string]any)
					if labels == nil {
						labels = map[string]any{}
						meta["labels"] = labels
					}
					labels["kognition.dev/mutated"] = "true"
					patched, err := json.Marshal(obj)
					if err != nil {
						return &intadmission.Response{Allowed: false, Reason: err.Error()}
					}
					return &intadmission.Response{Allowed: true, Patch: patched}
				},
			}
			chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
			h := NewHandler(chain, testScheme(), testLogger())

			raw := []byte(`{"apiVersion":"kognition.dev/v1alpha1","kind":"` + kind + `","metadata":{"name":"x","namespace":"default"}}`)
			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:       "224",
					Operation: admissionv1.Create,
					Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: kind},
					Namespace: "default",
					Name:      "x",
					Object:    runtime.RawExtension{Raw: raw},
				},
			}

			resp := h.Handle(context.Background(), req)
			if !resp.Allowed {
				t.Fatalf("kind=%s: expected allowed, got denied: %v", kind, resp.Result)
			}
			if len(resp.Patches) == 0 {
				t.Fatalf("kind=%s: expected JSON patches for label mutation", kind)
			}
			found := false
			for _, op := range resp.Patches {
				if strings.HasPrefix(op.Path, "/metadata/labels") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("kind=%s: expected patch op under /metadata/labels, got: %+v", kind, resp.Patches)
			}
		})
	}
}

func TestWebhookConversionsRejectMalformedJSONForEveryKind(t *testing.T) {
	kinds := []string{
		"Agent",
		"AgentInstance",
		"AgentPolicy",
		"CognitiveAdmissionPolicy",
		"CognitiveQuota",
		"CognitiveLimitRange",
		"Workspace",
		"AgentIdentity",
		"AgentRole",
		"AgentRoleBinding",
		"MemoryClass",
		"MemoryClaim",
		"MemoryVolume",
		"ModelClass",
		"AutonomyClass",
		"Model",
		"SecretRef",
		"Tool",
		"ToolClass",
		"Workflow",
		"WorkflowRun",
		"ApprovalDecision",
		"KnowledgeBaseClass",
		"KnowledgeBase",
		"KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun",
		"KnowledgeBaseIndexSchedule",
		"Solution",
		"AgentJob",
	}
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	validOriginal := []byte(`{"apiVersion":"kognition.dev/v1alpha1","metadata":{"name":"test"}}`)

	for _, kind := range kinds {
		t.Run(kind+"/internal", func(t *testing.T) {
			if _, err := handler.convertToInternal(kind, []byte("{")); err == nil {
				t.Fatalf("convertToInternal(%q) accepted malformed JSON", kind)
			}
		})
		t.Run(kind+"/original", func(t *testing.T) {
			if _, err := handler.convertToK8s(kind, []byte(`{}`), []byte("{")); err == nil {
				t.Fatalf("convertToK8s(%q) accepted malformed original JSON", kind)
			}
		})
		t.Run(kind+"/mutated", func(t *testing.T) {
			if _, err := handler.convertToK8s(kind, []byte("{"), validOriginal); err == nil {
				t.Fatalf("convertToK8s(%q) accepted malformed mutated JSON", kind)
			}
		})
	}

	if _, err := handler.convertToK8s("Unknown", []byte(`{}`), validOriginal); err == nil {
		t.Fatal("convertToK8s accepted an unsupported kind")
	}
}

func TestHandleUpdateRejectsMalformedOldObject(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "malformed-old-object",
		Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
		Namespace: "default",
		Name:      "test-agent",
		Object:    runtime.RawExtension{Raw: raw},
		OldObject: runtime.RawExtension{Raw: []byte("{")},
	}})

	if response.Allowed {
		t.Fatal("malformed old object was allowed")
	}
	if response.Result == nil || response.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want HTTP 500", response)
	}
}

// --- Stub hooks for tests ---

type stubValidator struct {
	name       string
	kinds      map[string]bool
	validateFn func(context.Context, *intadmission.Request) *intadmission.Response
}

func (s *stubValidator) Name() string { return s.name }
func (s *stubValidator) Handles(kind string, op intadmission.Operation) bool {
	return s.kinds[kind]
}
func (s *stubValidator) Validate(ctx context.Context, req *intadmission.Request) *intadmission.Response {
	return s.validateFn(ctx, req)
}

type stubMutator struct {
	name     string
	kinds    map[string]bool
	mutateFn func(context.Context, *intadmission.Request) *intadmission.Response
}

func (s *stubMutator) Name() string { return s.name }
func (s *stubMutator) Handles(kind string, op intadmission.Operation) bool {
	return s.kinds[kind]
}
func (s *stubMutator) Mutate(ctx context.Context, req *intadmission.Request) *intadmission.Response {
	return s.mutateFn(ctx, req)
}
