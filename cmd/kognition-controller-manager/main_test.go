// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

type callRecorder struct{ outcomes []cpi.CallOutcome }

func (r *callRecorder) RecordCall(outcome cpi.CallOutcome) { r.outcomes = append(r.outcomes, outcome) }

func TestNewCPIRegistryRegistersProductionDriversAndReportsCallOutcomes(t *testing.T) {
	registry := newCPIRegistry(nil)
	if got := len(registry.List()); got != 5 || registry.Observed() {
		t.Fatalf("drivers = %d, observed = %t; want 5 production drivers and no observation without a recorder", got, registry.Observed())
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	recorder := &callRecorder{}
	registry = newCPIRegistry(recorder)
	driver, ok := registry.Get("anthropic")
	if !ok || !registry.Observed() {
		t.Fatalf("anthropic registered = %t, observed = %t; want both true with a recorder", ok, registry.Observed())
	}
	if _, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model: "claude", Endpoint: server.URL, Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Complete() succeeded against a rate limiting provider")
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded %d outcomes, want 1: %+v", len(recorder.outcomes), recorder.outcomes)
	}
	got := recorder.outcomes[0]
	if got.Provider != "anthropic" || got.Operation != cpi.OperationComplete || got.Model != "claude" || got.Reason != cpi.ReasonRateLimited || !got.ProviderFault {
		t.Fatalf("outcome = %+v, want a provider-fault RateLimited complete call to claude through anthropic", got)
	}
}

type canaryOperationCapableStore struct {
	store.Store
	store.CanaryOperationStore
}

type callHealthCapableStore struct {
	store.Store
	store.ModelCallHealthStore
}

func runnableTypes(mgr *controller.Manager) []string {
	return runnableTypesIn(mgr, "runnables")
}

// runnableTypesIn lists the types of the manager's runnables of one kind: "runnables"
// stop with the controllers and "background" outlive them.
func runnableTypesIn(mgr *controller.Manager, field string) []string {
	runnables := reflect.ValueOf(mgr).Elem().FieldByName(field)
	types := make([]string, 0, runnables.Len())
	for index := 0; index < runnables.Len(); index++ {
		types = append(types, runnables.Index(index).Elem().Type().String())
	}
	return types
}

func TestPassiveModelHealthIsOnlyWiredForStoresThatCanShareIt(t *testing.T) {
	if newPassiveModelHealth(nil) != nil || newPassiveModelHealth(&canaryOperationCapableStore{}) != nil {
		t.Fatal("a store without passive health support must not enable it")
	}
	var disabled *passiveModelHealth
	base := &callRecorder{}
	if disabled.recorder(base) != cpi.CallRecorder(base) || disabled.recorder(nil) != nil {
		t.Fatal("a disabled passive health must leave the recorder unchanged")
	}
	disabled.register(nil, nil, nil, zap.NewNop())
}

func TestPassiveModelHealthRecordsOutcomesAndRegistersItsRunnables(t *testing.T) {
	st := &callHealthCapableStore{}
	passive := newPassiveModelHealth(st)
	if passive == nil {
		t.Fatal("a store that implements ModelCallHealthStore must enable passive health")
	}

	base := &callRecorder{}
	recorder := passive.recorder(base)
	recorder.RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Model: "claude"})
	if len(base.outcomes) != 1 || len(passive.tracker.Snapshot()) != 2 {
		t.Fatalf("outcomes reached the base recorder %d times and the tracker holds %d streams, want one outcome and the destination and credential streams of its success", len(base.outcomes), len(passive.tracker.Snapshot()))
	}
	if only := passive.recorder(nil); only != cpi.CallRecorder(passive.tracker) {
		t.Fatal("without a base recorder the tracker must be the recorder")
	}

	mgr := controller.NewManager(controller.ManagerConfig{Store: st, Logger: zap.NewNop()})
	modelController := mgr.Register(controller.ControllerConfig{Name: "model-controller", Kind: "Model", Reconciler: nil})
	passive.register(mgr, st, modelController, zap.NewNop())
	types := runnableTypes(mgr)
	background := runnableTypesIn(mgr, "background")
	if !slices.Contains(types, "*controller.ModelHealthWatcher") || slices.Contains(types, "*modelhealth.Reporter") {
		t.Fatalf("runnables = %v, want the watcher, which stops with the controllers, and not the reporter", types)
	}
	if len(background) != 1 || background[0] != "*modelhealth.Reporter" {
		t.Fatalf("background runnables = %v, want only the reporter, which must outlive the controllers to publish their last outcomes", background)
	}
}

func TestControllerDSN(t *testing.T) {
	t.Setenv("KOGNITION_DSN", "postgres://configured")
	if got := controllerDSN(); got != "postgres://configured" {
		t.Fatalf("configured DSN = %q", got)
	}
	t.Setenv("KOGNITION_DSN", "")
	if got := controllerDSN(); !strings.Contains(got, "localhost:5432/kognition") {
		t.Fatalf("default DSN = %q", got)
	}
}

func TestNewControllerLogger(t *testing.T) {
	logger := newControllerLogger()
	if logger == nil {
		t.Fatal("newControllerLogger() returned nil")
	}
	_ = logger.Sync()
}

func TestNewControllerManagerRegistersCompleteControllerRoster(t *testing.T) {
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	managerValue := reflect.ValueOf(mgr).Elem()
	controllers := managerValue.FieldByName("controllers")
	informers := managerValue.FieldByName("informers")

	expected := map[string]struct {
		kind    string
		workers int64
	}{
		"agent-controller":                            {kind: "Agent", workers: 2},
		"agent-instance-controller":                   {kind: "AgentInstance", workers: 2},
		"agent-instance-probe-controller":             {kind: "AgentInstance", workers: 2},
		"agent-job-controller":                        {kind: "AgentJob", workers: 2},
		"workflow-controller":                         {kind: "Workflow", workers: 2},
		"workflow-run-controller":                     {kind: "WorkflowRun", workers: 2},
		"evaluation-controller":                       {kind: "Evaluation", workers: 2},
		"evaluation-run-controller":                   {kind: "EvaluationRun", workers: 2},
		"prompt-controller":                           {kind: "Prompt", workers: 2},
		"memory-claim-controller":                     {kind: "MemoryClaim", workers: 2},
		"memory-class-controller":                     {kind: "MemoryClass", workers: 1},
		"memory-volume-controller":                    {kind: "MemoryVolume", workers: 2},
		"memory-grant-controller":                     {kind: "MemoryGrant", workers: 2},
		"knowledge-base-controller":                   {kind: "KnowledgeBase", workers: 2},
		"knowledge-base-claim-controller":             {kind: "KnowledgeBaseClaim", workers: 2},
		"knowledge-base-index-run-controller":         {kind: "KnowledgeBaseIndexRun", workers: 2},
		"knowledge-base-index-schedule-controller":    {kind: "KnowledgeBaseIndexSchedule", workers: 2},
		"tool-controller":                             {kind: "Tool", workers: 2},
		"tool-class-controller":                       {kind: "ToolClass", workers: 1},
		"tool-invocation-run-controller":              {kind: "ToolInvocationRun", workers: 2},
		"model-controller":                            {kind: "Model", workers: 2},
		"model-class-controller":                      {kind: "ModelClass", workers: 1},
		"memory-record-controller":                    {kind: "MemoryRecord", workers: 2},
		"memory-summarization-controller":             {kind: "Memory", workers: 2},
		"memory-summarization-from-record-controller": {kind: "MemoryRecord", workers: 2},
		"agent-autoscaler-controller":                 {kind: "AgentAutoscaler", workers: 2},
		"model-autoscaler-controller":                 {kind: "ModelAutoscaler", workers: 2},
		"intent-gateway-controller":                   {kind: "IntentGateway", workers: 2},
		"agent-service-controller":                    {kind: "AgentService", workers: 2},
		"solution-controller":                         {kind: "Solution", workers: 2},
		"secret-ref-controller":                       {kind: "SecretRef", workers: 2},
		"agent-policy-controller":                     {kind: "AgentPolicy", workers: 2},
		"cognitive-quota-controller":                  {kind: "CognitiveQuota", workers: 2},
		"trust-profile-controller":                    {kind: "TrustProfile", workers: 2},
		"autonomy-review-controller":                  {kind: "AutonomyReview", workers: 2},
		"agent-identity-controller":                   {kind: "AgentIdentity", workers: 2},
		"workspace-controller":                        {kind: "Workspace", workers: 2},
	}

	if controllers.Len() != len(expected) {
		t.Fatalf("registered controllers = %d, want %d", controllers.Len(), len(expected))
	}
	if informers.Len() != len(expected)-2 {
		t.Fatalf("shared informers = %d, want %d", informers.Len(), len(expected)-2)
	}

	for i := 0; i < controllers.Len(); i++ {
		controllerValue := controllers.Index(i).Elem()
		name := controllerValue.FieldByName("name").String()
		want, ok := expected[name]
		if !ok {
			t.Errorf("unexpected controller %q", name)
			continue
		}
		if got := controllerValue.FieldByName("kind").String(); got != want.kind {
			t.Errorf("controller %q kind = %q, want %q", name, got, want.kind)
		}
		if got := controllerValue.FieldByName("workers").Int(); got != want.workers {
			t.Errorf("controller %q workers = %d, want %d", name, got, want.workers)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Errorf("missing controllers: %v", expected)
	}
}

func TestNewControllerManagerRegistersCanaryRetention(t *testing.T) {
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	if err := configureCanaryOperationRetention(mgr, &canaryOperationCapableStore{}, zap.NewNop(), "72h"); err != nil {
		t.Fatalf("configureCanaryOperationRetention() error = %v", err)
	}
	runnables := reflect.ValueOf(mgr).Elem().FieldByName("runnables")
	if runnables.Len() != 1 {
		t.Fatalf("registered runnables = %d, want 1", runnables.Len())
	}
}

func TestNewControllerManagerRequiresAutomaticIdentity(t *testing.T) {
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	controllers := reflect.ValueOf(mgr).Elem().FieldByName("controllers")
	for index := 0; index < controllers.Len(); index++ {
		controllerValue := controllers.Index(index).Elem()
		if controllerValue.FieldByName("name").String() != "agent-instance-controller" {
			continue
		}
		reconciler := controllerValue.FieldByName("reconciler").Elem().Elem()
		if !reconciler.FieldByName("config").FieldByName("RequireAutomaticIdentity").Bool() {
			t.Fatal("standalone dispatch does not require an automatic managed identity")
		}
		return
	}
	t.Fatal("agent-instance-controller not found")
}

func TestNewControllerManagerDisablesCanaryRetention(t *testing.T) {
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	if err := configureCanaryOperationRetention(mgr, &canaryOperationCapableStore{}, zap.NewNop(), "0"); err != nil {
		t.Fatalf("configureCanaryOperationRetention() error = %v", err)
	}
	runnables := reflect.ValueOf(mgr).Elem().FieldByName("runnables")
	if runnables.Len() != 0 {
		t.Fatalf("registered runnables = %d, want 0", runnables.Len())
	}
}

func TestNewControllerManagerWiresStandaloneKnowledgeBaseIngester(t *testing.T) {
	t.Setenv("KOGNITION_KB_INGESTER_URL", "http://ingester.test:8082")
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	controllers := reflect.ValueOf(mgr).Elem().FieldByName("controllers")

	want := map[string]string{
		"knowledge-base-controller":           "launcher",
		"knowledge-base-index-run-controller": "launcher",
	}
	for index := 0; index < controllers.Len(); index++ {
		controllerValue := controllers.Index(index).Elem()
		name := controllerValue.FieldByName("name").String()
		fieldName, ok := want[name]
		if !ok {
			continue
		}
		reconciler := controllerValue.FieldByName("reconciler")
		if reconciler.Kind() == reflect.Interface {
			reconciler = reconciler.Elem()
		}
		if reconciler.Kind() == reflect.Pointer {
			reconciler = reconciler.Elem()
		}
		if launcher := reconciler.FieldByName(fieldName); !launcher.IsValid() || launcher.IsNil() {
			t.Errorf("controller %q has no standalone ingester launcher", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("ingester controllers not found: %v", want)
	}
}

func TestNewControllerManagerWiresMemoryServiceProvisioner(t *testing.T) {
	t.Setenv("KOGNITION_MEMORY_SERVICE_URL", "http://memory.test:8090")
	mgr := newControllerManager(nil, zap.NewNop(), nil, nil, nil)
	controllers := reflect.ValueOf(mgr).Elem().FieldByName("controllers")

	for index := 0; index < controllers.Len(); index++ {
		controllerValue := controllers.Index(index).Elem()
		if controllerValue.FieldByName("name").String() != "memory-claim-controller" {
			continue
		}
		reconciler := controllerValue.FieldByName("reconciler")
		if reconciler.Kind() == reflect.Interface {
			reconciler = reconciler.Elem()
		}
		provisioner := reconciler.Elem().FieldByName("provisioner")
		if !provisioner.IsValid() || provisioner.IsNil() {
			t.Fatal("memory-claim-controller has no configured memory service provisioner")
		}
		return
	}
	t.Fatal("memory-claim-controller not found")
}
