package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type WorkflowExecutorRecordStore struct {
	writer      client.Client
	reader      client.Reader
	namespace   string
	clock       func() (time.Time, error)
	submissions runtimeapproval.WorkflowRunSubmissionEvidenceAuthorizer
	finalizer   func(context.Context) (api.ControlPlaneRoleSubject, string, error)
}

var _ runtimeapproval.WorkflowExecutorBindingStore = (*WorkflowExecutorRecordStore)(nil)
var _ runtimeapproval.WorkflowExecutorBindingStore = (*StoreAdapter)(nil)

func NewWorkflowExecutorRecordStore(writer client.Client, reader client.Reader, namespace string, clock func() (time.Time, error)) (*WorkflowExecutorRecordStore, error) {
	if writer == nil || reader == nil || namespace == "" || clock == nil {
		return nil, store.ErrConflict
	}
	return &WorkflowExecutorRecordStore{writer: writer, reader: reader, namespace: namespace, clock: clock}, nil
}

func WorkflowExecutorBindingObjectName(namespace, name string) string {
	return runtimeapproval.WorkflowExecutorBindingObjectName(namespace, name)
}

func workflowRunAcceptanceObjectName(workspace, runUID string) string {
	return runtimeapproval.WorkflowRunAcceptanceObjectName(workspace, runUID)
}

func (storage *WorkflowExecutorRecordStore) CreateNativeWorkflowSubmissionReceipt(ctx context.Context, receipt runtimeapproval.NativeWorkflowSubmissionReceipt) error {
	now, err := storage.clock()
	if err != nil || runtimeapproval.ValidateNativeWorkflowSubmissionReceipt(receipt, now) != nil {
		return store.ErrConflict
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return store.ErrConflict
	}
	immutable := true
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: storage.namespace,
		Name:   runtimeapproval.NativeWorkflowSubmissionObjectName(receipt.Run.Namespace, receipt.Run.UID),
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-submission"}},
		Immutable: &immutable, Data: map[string]string{"receipt.json": string(raw)}}
	if err := storage.writer.Create(ctx, object); apierrors.IsAlreadyExists(err) {
		return store.ErrAlreadyExists
	} else {
		return err
	}
}

func (storage *WorkflowExecutorRecordStore) GetNativeWorkflowSubmissionReceipt(ctx context.Context, workspace, runUID string) (runtimeapproval.NativeWorkflowSubmissionReceipt, error) {
	object := &corev1.ConfigMap{}
	name := runtimeapproval.NativeWorkflowSubmissionObjectName(workspace, runUID)
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: storage.namespace, Name: name}, object); apierrors.IsNotFound(err) {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, store.ErrNotFound
	} else if err != nil {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, err
	}
	receipt, err := runtimeapproval.DecodeNativeWorkflowSubmissionReceipt([]byte(object.Data["receipt.json"]))
	version, versionErr := strconv.ParseInt(object.ResourceVersion, 10, 64)
	if err != nil || versionErr != nil || version <= 0 || object.UID == "" || object.Namespace != storage.namespace || object.Name != name ||
		object.DeletionTimestamp != nil || object.Immutable == nil || !*object.Immutable || len(object.Data) != 1 || len(object.BinaryData) != 0 ||
		object.Labels["kognition.dev/internal-record"] != "workflow-run-submission" || receipt.Run.Namespace != workspace || receipt.Run.UID != runUID {
		return runtimeapproval.NativeWorkflowSubmissionReceipt{}, errors.Join(store.ErrConflict, err, versionErr)
	}
	return receipt, nil
}

func bindingConfigJSON(authority runtimeapproval.WorkflowExecutorAuthority) ([]byte, error) {
	authority.Reference.UID, authority.Reference.ResourceVersion = "", 0
	return json.Marshal(authority)
}

func (storage *WorkflowExecutorRecordStore) validateWorkflowDefinition(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) error {
	workflow := &k8s.Workflow{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: authority.Workflow.Namespace, Name: authority.Workflow.Name}, workflow); err != nil {
		return err
	}
	raw, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		return err
	}
	if err := runtimeapproval.ValidateWorkflowExecutorAuthorityDefinition(authority, raw); err != nil {
		return errors.Join(store.ErrConflict, err)
	}
	return nil
}

func (storage *WorkflowExecutorRecordStore) CreateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if err := runtimeapproval.ValidateWorkflowExecutorAuthority(authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	if err := storage.validateWorkflowDefinition(ctx, authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	raw, err := bindingConfigJSON(authority)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: storage.namespace,
		Name:   WorkflowExecutorBindingObjectName(authority.Reference.Namespace, authority.Reference.Name),
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-executor-binding"}}, Data: map[string]string{"binding.json": string(raw)}}
	if err := storage.writer.Create(ctx, object); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrAlreadyExists
		}
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	reference := authority.Reference
	reference.UID, reference.ResourceVersion = "", 0
	return storage.GetWorkflowExecutorAuthority(ctx, reference)
}

func (storage *WorkflowExecutorRecordStore) readAuthority(ctx context.Context, reference api.RuntimeApprovalReference) (*corev1.ConfigMap, runtimeapproval.WorkflowExecutorAuthority, error) {
	if reference.Kind != "WorkflowExecutionBinding" || reference.Namespace == "" || reference.Name == "" {
		return nil, runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
	}
	object := &corev1.ConfigMap{}
	err := storage.reader.Get(ctx, client.ObjectKey{Namespace: storage.namespace, Name: WorkflowExecutorBindingObjectName(reference.Namespace, reference.Name)}, object)
	if apierrors.IsNotFound(err) {
		return nil, runtimeapproval.WorkflowExecutorAuthority{}, store.ErrNotFound
	}
	if err != nil {
		return nil, runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	authority, err := runtimeapproval.DecodeWorkflowExecutorAuthority([]byte(object.Data["binding.json"]))
	version, versionErr := strconv.ParseInt(object.ResourceVersion, 10, 64)
	if err != nil || versionErr != nil || version <= 0 || object.UID == "" || object.DeletionTimestamp != nil ||
		object.Namespace != storage.namespace || object.Name != WorkflowExecutorBindingObjectName(reference.Namespace, reference.Name) ||
		len(object.Data) != 1 || len(object.BinaryData) != 0 || object.Immutable != nil && *object.Immutable ||
		object.Labels["kognition.dev/internal-record"] != "workflow-executor-binding" ||
		authority.Reference.UID != "" || authority.Reference.ResourceVersion != 0 ||
		authority.Reference.Namespace != reference.Namespace || authority.Reference.Name != reference.Name ||
		reference.UID != "" && reference.UID != string(object.UID) || reference.ResourceVersion != 0 && reference.ResourceVersion != version {
		return nil, runtimeapproval.WorkflowExecutorAuthority{}, errors.Join(store.ErrConflict, err, versionErr)
	}
	authority.Reference.UID, authority.Reference.ResourceVersion = string(object.UID), version
	return object, authority, nil
}

func (storage *WorkflowExecutorRecordStore) GetWorkflowExecutorAuthority(ctx context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error) {
	_, authority, err := storage.readAuthority(ctx, reference)
	return authority, err
}

func (storage *WorkflowExecutorRecordStore) UpdateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if err := runtimeapproval.ValidateWorkflowExecutorAuthority(authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	object, previous, err := storage.readAuthority(ctx, authority.Reference)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	if previous.Workflow != authority.Workflow || previous.WorkflowRevisionDigest != authority.WorkflowRevisionDigest || previous.Executor != authority.Executor {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
	}
	if authority.Enabled {
		if err := storage.validateWorkflowDefinition(ctx, authority); err != nil {
			return runtimeapproval.WorkflowExecutorAuthority{}, err
		}
	}
	raw, err := bindingConfigJSON(authority)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	object.Data["binding.json"] = string(raw)
	if err := storage.writer.Update(ctx, object); err != nil {
		if apierrors.IsConflict(err) {
			return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
		}
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	reference := authority.Reference
	reference.ResourceVersion = 0
	return storage.GetWorkflowExecutorAuthority(ctx, reference)
}

func (storage *WorkflowExecutorRecordStore) CreateWorkflowRunAcceptance(ctx context.Context, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	if err := runtimeapproval.ValidateWorkflowRunAcceptance(acceptance); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	run := &k8s.WorkflowRun{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: acceptance.Workspace, Name: acceptance.Run.Name}, run); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if run.DeletionTimestamp != nil || string(run.UID) != acceptance.RunUID || run.Spec.WorkflowRef.Name != acceptance.Workflow.Name ||
		run.ResourceVersion != strconv.FormatInt(acceptance.Run.ResourceVersion, 10) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	authority, err := storage.GetWorkflowExecutorAuthority(ctx, acceptance.ExecutionBinding)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	workflow := &k8s.Workflow{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: acceptance.Workflow.Namespace, Name: acceptance.Workflow.Name}, workflow); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	workflowJSON, workflowErr := json.Marshal(convert.WorkflowFromK8s(workflow))
	runJSON, runErr := json.Marshal(convert.WorkflowRunFromK8s(run))
	if workflowErr != nil || runErr != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(workflowErr, runErr)
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	now, err := storage.clock()
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	return storage.persistWorkflowRunAcceptance(ctx, acceptance)
}

func (storage *WorkflowExecutorRecordStore) persistWorkflowRunAcceptance(ctx context.Context, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	raw, err := json.Marshal(acceptance)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	immutable := true
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: storage.namespace,
		Name:   workflowRunAcceptanceObjectName(acceptance.Workspace, acceptance.RunUID),
		Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-acceptance"}}, Immutable: &immutable,
		Data: map[string]string{"acceptance.json": string(raw)}}
	if err := storage.writer.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	retained, err := storage.GetWorkflowRunAcceptance(ctx, acceptance.Workspace, acceptance.RunUID)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if !reflect.DeepEqual(retained, acceptance) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	return retained, nil
}

func (storage *WorkflowExecutorRecordStore) ConfigureWorkflowRunSubmission(authorizer runtimeapproval.WorkflowRunSubmissionEvidenceAuthorizer, finalizer func(context.Context) (api.ControlPlaneRoleSubject, string, error)) error {
	if authorizer == nil || finalizer == nil {
		return runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	storage.submissions, storage.finalizer = authorizer, finalizer
	return nil
}

func (storage *WorkflowExecutorRecordStore) FinalizeWorkflowRunSubmission(ctx context.Context, workspace, name string) (runtimeapproval.WorkflowRunAcceptance, error) {
	if storage.submissions == nil || storage.finalizer == nil {
		return runtimeapproval.WorkflowRunAcceptance{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	executor, controllerUID, err := storage.finalizer(ctx)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	run := &k8s.WorkflowRun{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: workspace, Name: name}, run); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	runJSON, err := json.Marshal(convert.WorkflowRunFromK8s(run))
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	acceptance, err := storage.submissions.AuthorizeAcceptance(ctx, runJSON)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	authority, err := storage.GetWorkflowExecutorAuthority(ctx, acceptance.ExecutionBinding)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	approvedController := false
	for _, approved := range authority.ControllerUIDs {
		approvedController = approvedController || approved == controllerUID
	}
	if !approvedController || controllerUID == "" || executor != acceptance.Executor || executor != authority.Executor || acceptance.Workspace != workspace || acceptance.Run.Name != name {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	workflow := &k8s.Workflow{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: authority.Workflow.Namespace, Name: authority.Workflow.Name}, workflow); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	workflowJSON, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceEvidenceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	current := &k8s.WorkflowRun{}
	if err := storage.reader.Get(ctx, client.ObjectKey{Namespace: workspace, Name: name}, current); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	currentJSON, err := json.Marshal(convert.WorkflowRunFromK8s(current))
	if err != nil || string(currentJSON) != string(runJSON) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	verified, err := storage.submissions.AuthorizeAcceptance(ctx, currentJSON)
	if err != nil || !reflect.DeepEqual(verified, acceptance) {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	currentAuthority, err := storage.GetWorkflowExecutorAuthority(ctx, acceptance.ExecutionBinding)
	if err != nil || !reflect.DeepEqual(currentAuthority, authority) {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	currentExecutor, currentControllerUID, err := storage.finalizer(ctx)
	if err != nil || currentExecutor != executor || currentControllerUID != controllerUID {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	now, err := storage.clock()
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := ctx.Err(); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	return storage.persistWorkflowRunAcceptance(ctx, acceptance)
}

func (storage *WorkflowExecutorRecordStore) GetWorkflowRunAcceptance(ctx context.Context, workspace, runUID string) (runtimeapproval.WorkflowRunAcceptance, error) {
	object := &corev1.ConfigMap{}
	err := storage.reader.Get(ctx, client.ObjectKey{Namespace: storage.namespace, Name: workflowRunAcceptanceObjectName(workspace, runUID)}, object)
	if apierrors.IsNotFound(err) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrNotFound
	}
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	acceptance, err := runtimeapproval.DecodeWorkflowRunAcceptance([]byte(object.Data["acceptance.json"]))
	version, versionErr := strconv.ParseInt(object.ResourceVersion, 10, 64)
	if err != nil || object.DeletionTimestamp != nil || object.Immutable == nil || !*object.Immutable ||
		versionErr != nil || version <= 0 || object.UID == "" || object.Namespace != storage.namespace || object.Name != workflowRunAcceptanceObjectName(workspace, runUID) ||
		len(object.Data) != 1 || len(object.BinaryData) != 0 ||
		object.Labels["kognition.dev/internal-record"] != "workflow-run-acceptance" ||
		acceptance.Workspace != workspace || acceptance.RunUID != runUID {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err, versionErr)
	}
	return acceptance, nil
}

func (storage *StoreAdapter) WorkflowExecutorClock(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if err := storage.RuntimeApprovalClockReady(); err != nil {
		return time.Time{}, err
	}
	now := storage.runtimeApprovalNow()
	if now.IsZero() {
		return time.Time{}, store.ErrConflict
	}
	return now, nil
}

func (storage *StoreAdapter) ConfigureWorkflowExecutorRecords(records *WorkflowExecutorRecordStore) error {
	if records == nil {
		return store.ErrConflict
	}
	storage.workflowExecutorRecords = records
	return nil
}

func (storage *StoreAdapter) ConfigureWorkflowRunSubmission(authorizer runtimeapproval.WorkflowRunSubmissionEvidenceAuthorizer) error {
	if storage.workflowExecutorRecords == nil || storage.workflowExecutorAuthorization == nil {
		return runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.ConfigureWorkflowRunSubmission(authorizer, func(ctx context.Context) (api.ControlPlaneRoleSubject, string, error) {
		user, err := storage.workflowExecutorUser(ctx)
		if err != nil {
			return api.ControlPlaneRoleSubject{}, "", err
		}
		return api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: user.Username}, storage.workflowExecutorAuthorization.WorkloadUID, nil
	})
}

func (storage *StoreAdapter) FinalizeWorkflowRunSubmission(ctx context.Context, workspace, name string) (runtimeapproval.WorkflowRunAcceptance, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowRunAcceptance{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.FinalizeWorkflowRunSubmission(ctx, workspace, name)
}

func (storage *StoreAdapter) CreateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.CreateWorkflowExecutorAuthority(ctx, authority)
}

func (storage *StoreAdapter) UpdateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.UpdateWorkflowExecutorAuthority(ctx, authority)
}

func (storage *StoreAdapter) GetWorkflowExecutorAuthority(ctx context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.GetWorkflowExecutorAuthority(ctx, reference)
}

func (storage *StoreAdapter) CreateWorkflowRunAcceptance(ctx context.Context, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowRunAcceptance{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.CreateWorkflowRunAcceptance(ctx, acceptance)
}

func (storage *StoreAdapter) GetWorkflowRunAcceptance(ctx context.Context, workspace, runUID string) (runtimeapproval.WorkflowRunAcceptance, error) {
	if storage.workflowExecutorRecords == nil {
		return runtimeapproval.WorkflowRunAcceptance{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	return storage.workflowExecutorRecords.GetWorkflowRunAcceptance(ctx, workspace, runUID)
}
