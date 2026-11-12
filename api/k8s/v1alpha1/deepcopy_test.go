// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"reflect"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRegisteredResourcesDeepCopyWithoutSharedStorage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("register API types: %v", err)
	}

	knownTypes := scheme.AllKnownTypes()
	registeredGVKs := make(map[schema.GroupVersionKind]bool)
	for gvk := range knownTypes {
		if gvk.Group == GroupVersion.Group && gvk.Version == GroupVersion.Version &&
			knownTypes[gvk].PkgPath() == reflect.TypeOf(Agent{}).PkgPath() {
			registeredGVKs[gvk] = true
		}
	}

	expectedObjects := []runtime.Object{
		&Agent{}, &AgentList{},
		&AgentInstance{}, &AgentInstanceList{},
		&AgentJob{}, &AgentJobList{},
		&ApprovalDecision{}, &ApprovalDecisionList{},
		&AgentPolicy{}, &AgentPolicyList{},
		&CognitiveAdmissionPolicy{}, &CognitiveAdmissionPolicyList{},
		&CognitiveQuota{}, &CognitiveQuotaList{},
		&CognitiveLimitRange{}, &CognitiveLimitRangeList{},
		&Workspace{}, &WorkspaceList{},
		&AgentIdentity{}, &AgentIdentityList{},
		&AgentRole{}, &AgentRoleList{},
		&AgentRoleBinding{}, &AgentRoleBindingList{},
		&KnowledgeBaseIndexRun{}, &KnowledgeBaseIndexRunList{},
		&KnowledgeBaseIndexSchedule{}, &KnowledgeBaseIndexScheduleList{},
		&KnowledgeBaseClass{}, &KnowledgeBaseClassList{},
		&KnowledgeBase{}, &KnowledgeBaseList{},
		&KnowledgeBindingRecord{}, &KnowledgeBindingRecordList{},
		&KnowledgeBaseClaim{}, &KnowledgeBaseClaimList{},
		&MemoryClass{}, &MemoryClassList{},
		&MemoryClaim{}, &MemoryClaimList{},
		&Memory{}, &MemoryList{},
		&MemoryRecord{}, &MemoryRecordList{},
		&MemoryGrant{}, &MemoryGrantList{},
		&MemoryVolume{}, &MemoryVolumeList{},
		&ModelClass{}, &ModelClassList{},
		&ModelGrant{}, &ModelGrantList{},
		&AutonomyClass{}, &AutonomyClassList{},
		&AutonomyReview{}, &AutonomyReviewList{},
		&TrustProfile{}, &TrustProfileList{},
		&TrustProfileIndex{}, &TrustProfileIndexList{},
		&RuntimeApproval{}, &RuntimeApprovalList{},
		&RuntimeApprovalTombstone{}, &RuntimeApprovalTombstoneList{},
		&RuntimeApprovalQuotaTicket{}, &RuntimeApprovalQuotaTicketList{},
		&AgentService{}, &AgentServiceList{},
		&AgentCommunicationPolicy{}, &AgentCommunicationPolicyList{},
		&IntentGateway{}, &IntentGatewayList{},
		&Prompt{}, &PromptList{},
		&Solution{}, &SolutionList{},
		&Evaluation{}, &EvaluationList{},
		&EvaluationRun{}, &EvaluationRunList{},
		&EvaluationDataset{}, &EvaluationDatasetList{},
		&CrossNamespaceGrant{}, &CrossNamespaceGrantList{},
		&Model{}, &ModelList{},
		&ModelInvocationProfile{}, &ModelInvocationProfileList{},
		&ModelAutoscaler{}, &ModelAutoscalerList{},
		&AgentAutoscaler{}, &AgentAutoscalerList{},
		&SecretRef{}, &SecretRefList{},
		&Tool{}, &ToolList{},
		&ToolClass{}, &ToolClassList{},
		&ToolGrant{}, &ToolGrantList{},
		&ToolInvocationRun{}, &ToolInvocationRunList{},
		&Workflow{}, &WorkflowList{},
		&WorkflowRun{}, &WorkflowRunList{},
	}
	gvks := make([]schema.GroupVersionKind, 0, len(expectedObjects))
	for _, object := range expectedObjects {
		kind := reflect.TypeOf(object).Elem().Name()
		gvk := GroupVersion.WithKind(kind)
		if !registeredGVKs[gvk] {
			t.Errorf("%s is not registered with AddToScheme", gvk)
		}
		delete(registeredGVKs, gvk)
		gvks = append(gvks, gvk)
	}
	if len(registeredGVKs) != 0 {
		t.Fatalf("AddToScheme contains untested API types: %v", registeredGVKs)
	}
	sort.Slice(gvks, func(i, j int) bool { return gvks[i].Kind < gvks[j].Kind })
	testedDeepCopyTypes := make(map[reflect.Type]bool)

	for _, gvk := range gvks {
		t.Run(gvk.Kind, func(t *testing.T) {
			object, err := scheme.New(gvk)
			if err != nil {
				t.Fatalf("construct %s: %v", gvk, err)
			}
			populateDeepCopyFixture(reflect.ValueOf(object), 0)

			copied := object.DeepCopyObject()
			if !reflect.DeepEqual(object, copied) {
				t.Fatalf("DeepCopyObject() changed %s value", gvk)
			}
			assertNoSharedStorage(t, reflect.ValueOf(object), reflect.ValueOf(copied), gvk.Kind)
			assertReachableDeepCopies(t, reflect.ValueOf(object), gvk.Kind, testedDeepCopyTypes, 0)

			nilObject := reflect.Zero(reflect.TypeOf(object)).Interface().(runtime.Object)
			if copied := nilObject.DeepCopyObject(); copied != nil {
				t.Fatalf("DeepCopyObject() on nil %s returned %T", gvk, copied)
			}
		})
	}
}

func assertReachableDeepCopies(t *testing.T, value reflect.Value, path string, tested map[reflect.Type]bool, depth int) {
	t.Helper()
	if !value.IsValid() || depth > 12 {
		return
	}
	if value.Kind() == reflect.Interface {
		if !value.IsNil() {
			assertReachableDeepCopies(t, value.Elem(), path, tested, depth+1)
		}
		return
	}
	if value.Kind() == reflect.Pointer {
		if !value.IsNil() {
			assertReachableDeepCopies(t, value.Elem(), path, tested, depth+1)
		}
		return
	}

	if value.Kind() == reflect.Struct && value.Type().PkgPath() == reflect.TypeOf(Agent{}).PkgPath() && value.CanAddr() && !tested[value.Type()] {
		deepCopy := value.Addr().MethodByName("DeepCopy")
		if deepCopy.IsValid() {
			tested[value.Type()] = true
			copied := deepCopy.Call(nil)[0]
			if !reflect.DeepEqual(value.Addr().Interface(), copied.Interface()) {
				t.Errorf("DeepCopy() changed %s value", path)
			}
			assertNoSharedStorage(t, value.Addr(), copied, path)

			nilReceiver := reflect.Zero(reflect.PointerTo(value.Type()))
			nilCopy := nilReceiver.MethodByName("DeepCopy").Call(nil)[0]
			if !nilCopy.IsNil() {
				t.Errorf("DeepCopy() on nil %s returned non-nil", value.Type())
			}
		}
	}

	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if value.Type().Field(index).PkgPath == "" {
				assertReachableDeepCopies(t, field, path+"."+value.Type().Field(index).Name, tested, depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			assertReachableDeepCopies(t, value.Index(index), path, tested, depth+1)
		}
	}
}

func populateDeepCopyFixture(value reflect.Value, depth int) {
	if !value.IsValid() || depth > 12 {
		return
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() && value.CanSet() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		if !value.IsNil() {
			populateDeepCopyFixture(value.Elem(), depth+1)
		}
	case reflect.Interface:
		if value.IsNil() && value.CanSet() {
			value.Set(reflect.ValueOf("fixture"))
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if field.CanSet() {
				populateDeepCopyFixture(field, depth+1)
			}
		}
	case reflect.Slice:
		if value.CanSet() {
			value.Set(reflect.MakeSlice(value.Type(), 1, 1))
			populateDeepCopyFixture(value.Index(0), depth+1)
		}
	case reflect.Map:
		if value.CanSet() && value.Type().Key().Kind() == reflect.String {
			key := reflect.ValueOf("fixture").Convert(value.Type().Key())
			element := reflect.New(value.Type().Elem()).Elem()
			populateDeepCopyFixture(element, depth+1)
			value.Set(reflect.MakeMap(value.Type()))
			value.SetMapIndex(key, element)
		}
	case reflect.String:
		if value.CanSet() {
			value.SetString("fixture")
		}
	case reflect.Bool:
		if value.CanSet() {
			value.SetBool(true)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if value.CanSet() {
			value.SetInt(1)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if value.CanSet() {
			value.SetUint(1)
		}
	case reflect.Float32, reflect.Float64:
		if value.CanSet() {
			value.SetFloat(1)
		}
	}
}

func assertNoSharedStorage(t *testing.T, original, copied reflect.Value, path string) {
	t.Helper()
	if !original.IsValid() || !copied.IsValid() {
		return
	}

	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() {
			return
		}
		if original.Pointer() == copied.Pointer() {
			t.Errorf("%s shares pointer storage", path)
			return
		}
		assertNoSharedStorage(t, original.Elem(), copied.Elem(), path)
	case reflect.Interface:
		if !original.IsNil() {
			assertNoSharedStorage(t, original.Elem(), copied.Elem(), path)
		}
	case reflect.Struct:
		for index := 0; index < original.NumField(); index++ {
			if original.Type().Field(index).PkgPath == "" {
				assertNoSharedStorage(t, original.Field(index), copied.Field(index), path+"."+original.Type().Field(index).Name)
			}
		}
	case reflect.Slice:
		if original.Len() > 0 {
			if original.Pointer() == copied.Pointer() {
				t.Errorf("%s shares slice storage", path)
				return
			}
			for index := 0; index < original.Len(); index++ {
				assertNoSharedStorage(t, original.Index(index), copied.Index(index), path)
			}
		}
	case reflect.Map:
		if original.IsNil() {
			return
		}
		if original.Pointer() == copied.Pointer() {
			t.Errorf("%s shares map storage", path)
			return
		}
		iterator := original.MapRange()
		for iterator.Next() {
			assertNoSharedStorage(t, iterator.Value(), copied.MapIndex(iterator.Key()), path)
		}
	}
}

// TestAgentDeepCopy verifies that DeepCopy produces an independent copy.
func TestAgentDeepCopy(t *testing.T) {
	replicas := int32(3)
	maxTokens := int64(4096)
	original := &Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "ns"},
		Spec: AgentSpec{
			ModelRef:     LocalObjectReference{Name: "model-1"},
			Instructions: AgentInstructions{Inline: "original instructions"},
			Tools: []AgentToolRef{
				{Ref: LocalObjectReference{Name: "tool-1"}, Required: true},
			},
			Memory: &AgentMemorySpec{
				Claims: []LocalObjectReference{{Name: "mem-1"}},
			},
			Runtime: &AgentRuntimeSpec{
				Class:     "gpu",
				Timeout:   "30s",
				MaxTokens: &maxTokens,
			},
			Companions:          []LocalObjectReference{{Name: "companion-1"}},
			KnowledgeBaseClaims: []KnowledgeBaseClaimRef{{Name: "kb-claim-1"}},
			AutonomyClassRef:    &LocalObjectReference{Name: "class-1"},
			Replicas:            &replicas,
			Probes: &AgentProbeSpec{
				HealthProbe: &AgentHealthProbe{IntervalSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3},
			},
			RestartLadder: &RestartLadder{
				Steps: []RestartLadderStep{{Action: "restart", MaxAttempts: 3}},
			},
		},
		Status: AgentStatus{
			Phase:         "Running",
			Replicas:      3,
			ReadyReplicas: 2,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"},
			},
		},
	}

	copied := original.DeepCopy()

	// Mutate the copy and verify the original is unchanged.
	copied.Name = "agent-2"
	copied.Spec.ModelRef.Name = "model-2"
	copied.Spec.Instructions.Inline = "modified"
	copied.Spec.Tools[0].Ref.Name = "tool-2"
	copied.Spec.Memory.Claims[0].Name = "mem-2"
	copied.Spec.Runtime.Class = "cpu"
	*copied.Spec.Runtime.MaxTokens = 8192
	copied.Spec.Companions[0].Name = "companion-2"
	copied.Spec.KnowledgeBaseClaims[0].Name = "kb-claim-2"
	copied.Spec.AutonomyClassRef.Name = "class-2"
	*copied.Spec.Replicas = 5
	copied.Spec.Probes.HealthProbe.IntervalSeconds = 99
	copied.Spec.RestartLadder.Steps[0].Action = "alert"
	copied.Status.Phase = "Failed"
	copied.Status.Conditions[0].Reason = "NotOK"

	// Verify original is untouched.
	if original.Name != "agent-1" {
		t.Errorf("Name mutated: %q", original.Name)
	}
	if original.Spec.ModelRef.Name != "model-1" {
		t.Errorf("ModelRef.Name mutated: %q", original.Spec.ModelRef.Name)
	}
	if original.Spec.Instructions.Inline != "original instructions" {
		t.Errorf("Instructions.Inline mutated: %q", original.Spec.Instructions.Inline)
	}
	if original.Spec.Tools[0].Ref.Name != "tool-1" {
		t.Errorf("Tools[0].Ref.Name mutated: %q", original.Spec.Tools[0].Ref.Name)
	}
	if original.Spec.Memory.Claims[0].Name != "mem-1" {
		t.Errorf("Memory.Claims[0].Name mutated: %q", original.Spec.Memory.Claims[0].Name)
	}
	if original.Spec.Runtime.Class != "gpu" {
		t.Errorf("Runtime.Class mutated: %q", original.Spec.Runtime.Class)
	}
	if *original.Spec.Runtime.MaxTokens != 4096 {
		t.Errorf("Runtime.MaxTokens mutated: %d", *original.Spec.Runtime.MaxTokens)
	}
	if original.Spec.Companions[0].Name != "companion-1" {
		t.Errorf("Companions[0].Name mutated: %q", original.Spec.Companions[0].Name)
	}
	if original.Spec.KnowledgeBaseClaims[0].Name != "kb-claim-1" {
		t.Errorf("KnowledgeBaseClaims[0].Name mutated: %q", original.Spec.KnowledgeBaseClaims[0].Name)
	}
	if original.Spec.AutonomyClassRef.Name != "class-1" {
		t.Errorf("AutonomyClassRef.Name mutated: %q", original.Spec.AutonomyClassRef.Name)
	}
	if *original.Spec.Replicas != 3 {
		t.Errorf("Replicas mutated: %d", *original.Spec.Replicas)
	}
	if original.Spec.Probes.HealthProbe.IntervalSeconds != 10 {
		t.Errorf("Probes.HealthProbe.IntervalSeconds mutated: %d", original.Spec.Probes.HealthProbe.IntervalSeconds)
	}
	if original.Spec.RestartLadder.Steps[0].Action != "restart" {
		t.Errorf("RestartLadder.Steps[0].Action mutated: %q", original.Spec.RestartLadder.Steps[0].Action)
	}
	if original.Status.Phase != "Running" {
		t.Errorf("Status.Phase mutated: %q", original.Status.Phase)
	}
	if original.Status.Conditions[0].Reason != "OK" {
		t.Errorf("Status.Conditions[0].Reason mutated: %q", original.Status.Conditions[0].Reason)
	}
}

// TestAgentInstanceDeepCopy verifies AgentInstance DeepCopy independence.
func TestAgentInstanceDeepCopy(t *testing.T) {
	now := metav1.Now()
	scheduleSlot := "2026-03-19T12:00:00Z"
	revision := int64(7)
	original := &AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst-1"},
		Spec: AgentInstanceSpec{
			AgentRef:       LocalObjectReference{Name: "agent-1"},
			Revision:       5,
			SessionContext: map[string]string{"key": "value"},
		},
		Status: AgentInstanceStatus{
			Phase:      "Running",
			SessionID:  "sess-1",
			StartTime:  &now,
			TokenUsage: &TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ToolCalls:  2,
			ProbeStatus: &ProbeStatus{
				LastReasoningScheduleSlot: &scheduleSlot,
				ReasoningBindings: []CanaryResourceBinding{
					{Kind: "Model", Name: "model-1", Revision: &revision},
				},
				LastReasoningResult: &ReasoningCanaryResult{
					OperationID: "operation-1",
					Outcome:     CanaryOutcomePassed,
				},
			},
		},
	}

	copied := original.DeepCopy()

	// Mutate copy.
	copied.Spec.SessionContext["key"] = "modified"
	copied.Spec.SessionContext["new"] = "extra"
	copied.Status.TokenUsage.TotalTokens = 999
	copied.Status.SessionID = "sess-2"
	*copied.Status.ProbeStatus.LastReasoningScheduleSlot = "2026-03-19T12:05:00Z"
	copied.Status.ProbeStatus.ReasoningBindings[0].Name = "model-2"
	*copied.Status.ProbeStatus.ReasoningBindings[0].Revision = 8
	copied.Status.ProbeStatus.LastReasoningResult.OperationID = "operation-2"

	// Verify original.
	if original.Spec.SessionContext["key"] != "value" {
		t.Errorf("SessionContext[key] mutated: %q", original.Spec.SessionContext["key"])
	}
	if _, ok := original.Spec.SessionContext["new"]; ok {
		t.Error("SessionContext[new] leaked into original")
	}
	if original.Status.TokenUsage.TotalTokens != 150 {
		t.Errorf("TokenUsage.TotalTokens mutated: %d", original.Status.TokenUsage.TotalTokens)
	}
	if original.Status.SessionID != "sess-1" {
		t.Errorf("SessionID mutated: %q", original.Status.SessionID)
	}
	if *original.Status.ProbeStatus.LastReasoningScheduleSlot != scheduleSlot {
		t.Errorf("LastReasoningScheduleSlot mutated: %q", *original.Status.ProbeStatus.LastReasoningScheduleSlot)
	}
	if original.Status.ProbeStatus.ReasoningBindings[0].Name != "model-1" {
		t.Errorf("ReasoningBindings[0].Name mutated: %q", original.Status.ProbeStatus.ReasoningBindings[0].Name)
	}
	if *original.Status.ProbeStatus.ReasoningBindings[0].Revision != revision {
		t.Errorf("ReasoningBindings[0].Revision mutated: %d", *original.Status.ProbeStatus.ReasoningBindings[0].Revision)
	}
	if original.Status.ProbeStatus.LastReasoningResult.OperationID != "operation-1" {
		t.Errorf("LastReasoningResult.OperationID mutated: %q", original.Status.ProbeStatus.LastReasoningResult.OperationID)
	}
}

// TestToolDeepCopy verifies Tool DeepCopy independence.
func TestToolDeepCopy(t *testing.T) {
	original := &Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-1"},
		Spec: ToolSpec{
			ClassName: "class-1",
			Protocol:  ToolProtocolMCP,
			Metadata:  map[string]string{"key": "val"},
			Schema: map[string]any{
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
			},
			InputSchema: map[string]any{
				"items": map[string]any{"type": "number"},
			},
			Sandbox:    &ToolSandboxSpec{CPUMilli: 500, MemoryMB: 256},
			SecretRefs: []ToolSecretRef{{Name: "secret-1", Key: "k"}},
		},
		Status: ToolStatus{
			Phase: "Ready",
			Ready: true,
			Conditions: []metav1.Condition{
				{Type: "Available", Status: metav1.ConditionTrue},
			},
			CompiledMCP: &MCPToolDefinition{
				Name:        "tool-1",
				InputSchema: map[string]any{"properties": map[string]any{"path": map[string]any{"type": "string"}}},
				Transport:   TransportHTTP,
			},
		},
	}

	copied := original.DeepCopy()

	// Mutate copy.
	copied.Spec.Metadata["key"] = "modified"
	copied.Spec.Schema["properties"].(map[string]any)["query"].(map[string]any)["type"] = "integer"
	copied.Spec.InputSchema["items"].(map[string]any)["type"] = "string"
	copied.Spec.Sandbox.CPUMilli = 1000
	copied.Spec.SecretRefs[0].Name = "secret-2"
	copied.Status.Conditions[0].Status = metav1.ConditionFalse
	copied.Status.CompiledMCP.InputSchema["properties"].(map[string]any)["path"].(map[string]any)["type"] = "number"

	// Verify original.
	if original.Spec.Metadata["key"] != "val" {
		t.Errorf("Metadata[key] mutated: %q", original.Spec.Metadata["key"])
	}
	if original.Spec.Schema["properties"].(map[string]any)["query"].(map[string]any)["type"] != "string" {
		t.Error("nested Schema mutation leaked into original")
	}
	if original.Spec.InputSchema["items"].(map[string]any)["type"] != "number" {
		t.Error("nested InputSchema mutation leaked into original")
	}
	if original.Spec.Sandbox.CPUMilli != 500 {
		t.Errorf("Sandbox.CPUMilli mutated: %d", original.Spec.Sandbox.CPUMilli)
	}
	if original.Spec.SecretRefs[0].Name != "secret-1" {
		t.Errorf("SecretRefs[0].Name mutated: %q", original.Spec.SecretRefs[0].Name)
	}
	if original.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Conditions[0].Status mutated: %q", original.Status.Conditions[0].Status)
	}
	if original.Status.CompiledMCP.InputSchema["properties"].(map[string]any)["path"].(map[string]any)["type"] != "string" {
		t.Error("nested CompiledMCP.InputSchema mutation leaked into original")
	}
}

// TestToolClassDeepCopy verifies ToolClass DeepCopy independence.
func TestToolClassDeepCopy(t *testing.T) {
	original := &ToolClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1"},
		Spec: ToolClassSpec{
			Provisioner:     "mcp",
			Parameters:      map[string]string{"region": "us-east"},
			ExecutionTier:   ExecutionTierPod,
			SandboxImage:    "registry.example/tool-probe:1.0",
			Transport:       TransportHTTP,
			SandboxDefaults: &ToolSandboxSpec{CPUMilli: 250},
			ResourceLimits:  &ToolClassResourceLimits{MaxCPUMilli: 1000, MaxMemoryMB: 512, MaxTimeout: "30s"},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Parameters["region"] = "eu-west"
	copied.Spec.SandboxDefaults.CPUMilli = 999
	copied.Spec.ResourceLimits.MaxCPUMilli = 2000

	if original.Spec.Parameters["region"] != "us-east" {
		t.Errorf("Parameters[region] mutated: %q", original.Spec.Parameters["region"])
	}
	if original.Spec.SandboxDefaults.CPUMilli != 250 {
		t.Errorf("SandboxDefaults.CPUMilli mutated: %d", original.Spec.SandboxDefaults.CPUMilli)
	}
	if original.Spec.ResourceLimits.MaxCPUMilli != 1000 {
		t.Errorf("ResourceLimits.MaxCPUMilli mutated: %d", original.Spec.ResourceLimits.MaxCPUMilli)
	}
}

// TestModelClassDeepCopy verifies ModelClass DeepCopy independence.
func TestModelClassDeepCopy(t *testing.T) {
	original := &ModelClass{
		ObjectMeta: metav1.ObjectMeta{Name: "mc-1"},
		Spec: ModelClassSpec{
			Provisioner:   "openai",
			Parameters:    map[string]string{"key": "val"},
			Modalities:    []string{"text", "image"},
			ContextWindow: 128000,
			HealthProbe: &ModelHealthProbeSpec{
				Endpoint:        "http://probe",
				IntervalSeconds: 60,
			},
		},
		Status: ModelClassStatus{
			Phase:       "Ready",
			Provisioned: true,
			Conditions: []metav1.Condition{
				{Type: "Healthy", Status: metav1.ConditionTrue},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Parameters["key"] = "modified"
	copied.Spec.Modalities[0] = "audio"
	copied.Spec.HealthProbe.IntervalSeconds = 120
	copied.Status.Conditions[0].Status = metav1.ConditionFalse

	if original.Spec.Parameters["key"] != "val" {
		t.Errorf("Parameters[key] mutated: %q", original.Spec.Parameters["key"])
	}
	if original.Spec.Modalities[0] != "text" {
		t.Errorf("Modalities[0] mutated: %q", original.Spec.Modalities[0])
	}
	if original.Spec.HealthProbe.IntervalSeconds != 60 {
		t.Errorf("HealthProbe.IntervalSeconds mutated: %d", original.Spec.HealthProbe.IntervalSeconds)
	}
	if original.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Status.Conditions[0].Status mutated: %q", original.Status.Conditions[0].Status)
	}
}

// TestMemoryClassDeepCopy verifies MemoryClass DeepCopy independence.
func TestMemoryClassDeepCopy(t *testing.T) {
	original := &MemoryClass{
		ObjectMeta: metav1.ObjectMeta{Name: "memc-1"},
		Spec: MemoryClassSpec{
			BackendType:   "pgvector",
			Parameters:    map[string]string{"host": "localhost"},
			ReclaimPolicy: ReclaimPolicyRetain,
			SummarizationConfig: &MemorySummarizationConfig{
				Enabled:          true,
				ModelRef:         &LocalObjectReference{Name: "summarizer"},
				ThresholdRecords: 8000,
			},
			ConsistencyMode: ConcurrencyModeLWW,
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Parameters["host"] = "remote"
	copied.Spec.SummarizationConfig.ThresholdRecords = 16000
	copied.Spec.SummarizationConfig.ModelRef.Name = "new-model"

	if original.Spec.Parameters["host"] != "localhost" {
		t.Errorf("Parameters[host] mutated: %q", original.Spec.Parameters["host"])
	}
	if original.Spec.SummarizationConfig.ThresholdRecords != 8000 {
		t.Errorf("ThresholdRecords mutated: %d", original.Spec.SummarizationConfig.ThresholdRecords)
	}
	if original.Spec.SummarizationConfig.ModelRef.Name != "summarizer" {
		t.Errorf("ModelRef.Name mutated: %q", original.Spec.SummarizationConfig.ModelRef.Name)
	}
}

// TestWorkflowDeepCopy verifies Workflow DeepCopy independence.
func TestWorkflowDeepCopy(t *testing.T) {
	original := &Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-1"},
		Spec: WorkflowSpec{
			Steps: []WorkflowStep{
				{Name: "step1", AgentRef: LocalObjectReference{Name: "agent-1"}},
			},
			Edges:         []WorkflowEdge{{From: "a", To: "b", Condition: "ok"}},
			ApprovalGates: []WorkflowApprovalGate{{From: "a", To: "b"}},
			Budgets:       &WorkflowBudget{MaxTokens: 50000, MaxDuration: "1h"},
		},
		Status: WorkflowStatus{
			Phase:          "Running",
			CompletedSteps: []string{"init"},
			ActiveStep:     "step1",
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Steps[0].Name = "step-modified"
	copied.Spec.Edges[0].Condition = "fail"
	copied.Spec.ApprovalGates[0].To = "c"
	copied.Spec.Budgets.MaxTokens = 100000
	copied.Status.CompletedSteps[0] = "modified"

	if original.Spec.Steps[0].Name != "step1" {
		t.Errorf("Steps[0].Name mutated: %q", original.Spec.Steps[0].Name)
	}
	if original.Spec.Edges[0].Condition != "ok" {
		t.Errorf("Edges[0].Condition mutated: %q", original.Spec.Edges[0].Condition)
	}
	if original.Spec.ApprovalGates[0].To != "b" {
		t.Errorf("ApprovalGates[0].To mutated: %q", original.Spec.ApprovalGates[0].To)
	}
	if original.Spec.Budgets.MaxTokens != 50000 {
		t.Errorf("Budgets.MaxTokens mutated: %d", original.Spec.Budgets.MaxTokens)
	}
	if original.Status.CompletedSteps[0] != "init" {
		t.Errorf("Status.CompletedSteps[0] mutated: %q", original.Status.CompletedSteps[0])
	}
}

// TestWorkflowRunDeepCopy verifies WorkflowRun DeepCopy independence.
func TestWorkflowRunDeepCopy(t *testing.T) {
	now := metav1.Now()
	original := &WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
		Spec: WorkflowRunSpec{
			WorkflowRef: LocalObjectReference{Name: "wf-1"},
			Input:       "original input",
			Parameters:  map[string]string{"p1": "v1"},
		},
		Status: WorkflowRunStatus{
			Phase:          "Running",
			CompletedSteps: []string{"step1"},
			StartTime:      &now,
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Parameters["p1"] = "modified"
	copied.Spec.Parameters["p2"] = "new"
	copied.Status.CompletedSteps[0] = "modified-step"

	if original.Spec.Parameters["p1"] != "v1" {
		t.Errorf("Parameters[p1] mutated: %q", original.Spec.Parameters["p1"])
	}
	if _, ok := original.Spec.Parameters["p2"]; ok {
		t.Error("Parameters[p2] leaked into original")
	}
	if original.Status.CompletedSteps[0] != "step1" {
		t.Errorf("CompletedSteps[0] mutated: %q", original.Status.CompletedSteps[0])
	}
}

// TestAgentPolicyDeepCopy verifies AgentPolicy DeepCopy independence.
func TestAgentPolicyDeepCopy(t *testing.T) {
	original := &AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "ap-1"},
		Spec: AgentPolicySpec{
			Rules:        "package policy",
			Enforce:      true,
			Scope:        PolicyScopeAgent,
			TargetAgents: []string{"agent-a", "agent-b"},
			ToolGrants: []PolicyToolGrant{
				{ToolRef: "web-search", Allow: true, Conditions: "admin"},
			},
			SafetyRules: []GovernanceRule{
				{Name: "no-pii", Rego: "deny { pii }", Severity: RuleSeverityError},
			},
			ComplianceRules: []GovernanceRule{
				{Name: "audit", Rego: "deny { no_audit }", Severity: RuleSeverityWarning},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.TargetAgents[0] = "modified"
	copied.Spec.ToolGrants[0].ToolRef = "modified-tool"
	copied.Spec.SafetyRules[0].Name = "modified-rule"
	copied.Spec.ComplianceRules[0].Rego = "modified-rego"

	if original.Spec.TargetAgents[0] != "agent-a" {
		t.Errorf("TargetAgents[0] mutated: %q", original.Spec.TargetAgents[0])
	}
	if original.Spec.ToolGrants[0].ToolRef != "web-search" {
		t.Errorf("ToolGrants[0].ToolRef mutated: %q", original.Spec.ToolGrants[0].ToolRef)
	}
	if original.Spec.SafetyRules[0].Name != "no-pii" {
		t.Errorf("SafetyRules[0].Name mutated: %q", original.Spec.SafetyRules[0].Name)
	}
	if original.Spec.ComplianceRules[0].Rego != "deny { no_audit }" {
		t.Errorf("ComplianceRules[0].Rego mutated: %q", original.Spec.ComplianceRules[0].Rego)
	}
}

// TestCognitiveQuotaDeepCopy verifies CognitiveQuota DeepCopy independence.
func TestCognitiveQuotaDeepCopy(t *testing.T) {
	original := &CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "cq-1"},
		Spec: CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 10, "tools": 50},
			TokenBudget: &TokenBudget{
				MaxInputTokens:  1000000,
				MaxOutputTokens: 500000,
				MaxTotalTokens:  1500000,
			},
			CostBudget: &CostBudget{
				MaxCostPerPeriod:      10000,
				MaxToolCallsPerPeriod: 5000,
				MaxModelTierSeconds:   map[string]int64{"premium": 3600},
			},
			ResetInterval: "24h",
		},
		Status: CognitiveQuotaStatus{
			Used: map[string]int64{"agents": 5},
			TokenUsage: &QuotaTokenUsage{
				InputTokens:  500000,
				OutputTokens: 200000,
				TotalTokens:  700000,
			},
			CostUsage: &QuotaCostUsage{
				TotalCost:             4500,
				ToolCalls:             2000,
				ModelTierSeconds:      map[string]int64{"premium": 1250},
				ModelTierMilliseconds: map[string]int64{"premium": 1250000},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Hard["agents"] = 99
	copied.Spec.Hard["new-key"] = 1
	copied.Spec.TokenBudget.MaxTotalTokens = 9999999
	copied.Spec.CostBudget.MaxCostPerPeriod = 99999
	copied.Spec.CostBudget.MaxModelTierSeconds["premium"] = 99999
	copied.Status.Used["agents"] = 99
	copied.Status.TokenUsage.TotalTokens = 9999999
	copied.Status.CostUsage.ToolCalls = 9999
	copied.Status.CostUsage.ModelTierSeconds["premium"] = 9999
	copied.Status.CostUsage.ModelTierMilliseconds["premium"] = 9999

	if original.Spec.Hard["agents"] != 10 {
		t.Errorf("Hard[agents] mutated: %d", original.Spec.Hard["agents"])
	}
	if _, ok := original.Spec.Hard["new-key"]; ok {
		t.Error("Hard[new-key] leaked into original")
	}
	if original.Spec.TokenBudget.MaxTotalTokens != 1500000 {
		t.Errorf("TokenBudget.MaxTotalTokens mutated: %d", original.Spec.TokenBudget.MaxTotalTokens)
	}
	if original.Spec.CostBudget.MaxCostPerPeriod != 10000 {
		t.Errorf("CostBudget.MaxCostPerPeriod mutated: %d", original.Spec.CostBudget.MaxCostPerPeriod)
	}
	if original.Status.Used["agents"] != 5 {
		t.Errorf("Status.Used[agents] mutated: %d", original.Status.Used["agents"])
	}
	if original.Status.TokenUsage.TotalTokens != 700000 {
		t.Errorf("Status.TokenUsage.TotalTokens mutated: %d", original.Status.TokenUsage.TotalTokens)
	}
	if original.Status.CostUsage.ToolCalls != 2000 {
		t.Errorf("Status.CostUsage.ToolCalls mutated: %d", original.Status.CostUsage.ToolCalls)
	}
}

// TestCognitiveLimitRangeDeepCopy verifies CognitiveLimitRange DeepCopy independence.
func TestCognitiveLimitRangeDeepCopy(t *testing.T) {
	original := &CognitiveLimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "clr-1"},
		Spec: CognitiveLimitRangeSpec{
			Limits: []LimitRangeItem{
				{
					Type:             "Agent",
					Default:          map[string]int64{"maxTokens": 4096},
					Max:              map[string]int64{"maxTokens": 128000},
					Min:              map[string]int64{"maxTokens": 100},
					TokensPerSession: 50000,
				},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.Limits[0].Default["maxTokens"] = 9999
	copied.Spec.Limits[0].Max["maxTokens"] = 9999
	copied.Spec.Limits[0].TokensPerSession = 9999

	if original.Spec.Limits[0].Default["maxTokens"] != 4096 {
		t.Errorf("Limits[0].Default[maxTokens] mutated: %d", original.Spec.Limits[0].Default["maxTokens"])
	}
	if original.Spec.Limits[0].Max["maxTokens"] != 128000 {
		t.Errorf("Limits[0].Max[maxTokens] mutated: %d", original.Spec.Limits[0].Max["maxTokens"])
	}
	// Scalar in slice struct is copied by value — verify via original slice length
	if original.Spec.Limits[0].TokensPerSession != 50000 {
		t.Errorf("Limits[0].TokensPerSession mutated: %d", original.Spec.Limits[0].TokensPerSession)
	}
}

// TestWorkspaceDeepCopy verifies Workspace DeepCopy independence.
func TestWorkspaceDeepCopy(t *testing.T) {
	original := &Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1"},
		Spec: WorkspaceSpec{
			DisplayName:            "My Workspace",
			RequireSignedArtifacts: true,
			Retention: &RetentionPolicy{
				TraceTTL:    "720h",
				AuditLogTTL: "2160h",
			},
		},
		Status: WorkspaceStatus{Phase: "Active"},
	}

	copied := original.DeepCopy()
	copied.Spec.Retention.TraceTTL = "1h"
	copied.Spec.DisplayName = "Modified"

	if original.Spec.Retention.TraceTTL != "720h" {
		t.Errorf("Retention.TraceTTL mutated: %q", original.Spec.Retention.TraceTTL)
	}
	if original.Spec.DisplayName != "My Workspace" {
		t.Errorf("DisplayName mutated: %q", original.Spec.DisplayName)
	}
}

// TestAgentIdentityDeepCopy verifies AgentIdentity DeepCopy independence.
func TestAgentIdentityDeepCopy(t *testing.T) {
	original := &AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-1"},
		Spec: AgentIdentitySpec{
			SPIFFEID: "spiffe://cluster.local/ns/default/agent/a",
			AgentRef: LocalObjectReference{Name: "agent-a"},
		},
		Status: AgentIdentityStatus{Phase: "Issued", WorkloadIdentity: &WorkloadIdentityStatus{AgentUID: "agent-uid", Ready: true}},
	}

	copied := original.DeepCopy()
	copied.Spec.AgentRef.Name = "modified"
	copied.Status.Phase = "Revoked"
	copied.Status.WorkloadIdentity.AgentUID = "recreated-uid"
	copied.Status.WorkloadIdentity.Ready = false

	if original.Spec.AgentRef.Name != "agent-a" {
		t.Errorf("AgentRef.Name mutated: %q", original.Spec.AgentRef.Name)
	}
	if original.Status.Phase != "Issued" {
		t.Errorf("Status.Phase mutated: %q", original.Status.Phase)
	}
	if original.Status.WorkloadIdentity.AgentUID != "agent-uid" || !original.Status.WorkloadIdentity.Ready {
		t.Fatal("copy mutated original certificate evidence")
	}
}

// TestAgentRoleDeepCopy verifies AgentRole DeepCopy independence.
func TestAgentRoleDeepCopy(t *testing.T) {
	original := &AgentRole{
		ObjectMeta: metav1.ObjectMeta{Name: "role-1"},
		Rules: []PolicyRule{
			{Resources: []string{"tools", "agents"}, Verbs: []string{"get", "list"}},
		},
	}

	copied := original.DeepCopy()
	copied.Rules[0].Resources[0] = "modified"
	copied.Rules[0].Verbs[0] = "modified"

	if original.Rules[0].Resources[0] != "tools" {
		t.Errorf("Rules[0].Resources[0] mutated: %q", original.Rules[0].Resources[0])
	}
	if original.Rules[0].Verbs[0] != "get" {
		t.Errorf("Rules[0].Verbs[0] mutated: %q", original.Rules[0].Verbs[0])
	}
}

// TestAgentRoleBindingDeepCopy verifies AgentRoleBinding DeepCopy independence.
func TestAgentRoleBindingDeepCopy(t *testing.T) {
	original := &AgentRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rb-1"},
		Spec: AgentRoleBindingSpec{
			RoleRef: LocalObjectReference{Name: "tool-user"},
			Subjects: []Subject{
				{Kind: "Agent", Name: "agent-a"},
				{Kind: "AgentIdentity", Name: "id-b"},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.RoleRef.Name = "modified"
	copied.Spec.Subjects[0].Name = "modified"

	if original.Spec.RoleRef.Name != "tool-user" {
		t.Errorf("RoleRef.Name mutated: %q", original.Spec.RoleRef.Name)
	}
	if original.Spec.Subjects[0].Name != "agent-a" {
		t.Errorf("Subjects[0].Name mutated: %q", original.Spec.Subjects[0].Name)
	}
}

// TestModelDeepCopy verifies Model DeepCopy independence across slices, maps, and pointers.
func TestModelDeepCopy(t *testing.T) {
	rateLimit := int64(100)
	original := &Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-1", Namespace: "ns"},
		Spec: ModelSpec{
			Provider:      "openai-compatible",
			ClassName:     "fast",
			ModelID:       "gpt-4o",
			Endpoint:      "https://api.example.com",
			Capabilities:  []string{"chat", "tool-calling"},
			CredentialRef: &LocalObjectReference{Name: "creds"},
			RateLimit:     &rateLimit,
			Parameters:    map[string]string{"temperature": "0.7", "max_tokens": "1024"},
			Signature:     "sig-abc",
		},
		Status: ModelStatus{
			Phase:        "Ready",
			Available:    true,
			Latency:      "120ms",
			LastCheck:    "2024-01-01T00:00:00Z",
			Capabilities: []string{"chat"},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"},
			},
		},
	}

	copied := original.DeepCopy()

	copied.Name = "model-2"
	copied.Spec.Provider = "azure-openai"
	copied.Spec.Capabilities[0] = "embedding"
	copied.Spec.CredentialRef.Name = "creds-2"
	*copied.Spec.RateLimit = 999
	copied.Spec.Parameters["temperature"] = "0.1"
	copied.Spec.Parameters["new"] = "added"
	copied.Status.Phase = "Failed"
	copied.Status.Capabilities[0] = "mutated"
	copied.Status.Conditions[0].Reason = "NotOK"

	if original.Name != "model-1" {
		t.Errorf("Name mutated: %q", original.Name)
	}
	if original.Spec.Provider != "openai-compatible" {
		t.Errorf("Provider mutated: %q", original.Spec.Provider)
	}
	if original.Spec.Capabilities[0] != "chat" {
		t.Errorf("Spec.Capabilities[0] mutated: %q", original.Spec.Capabilities[0])
	}
	if original.Spec.CredentialRef.Name != "creds" {
		t.Errorf("CredentialRef.Name mutated: %q", original.Spec.CredentialRef.Name)
	}
	if *original.Spec.RateLimit != 100 {
		t.Errorf("RateLimit mutated: %d", *original.Spec.RateLimit)
	}
	if original.Spec.Parameters["temperature"] != "0.7" {
		t.Errorf("Parameters[temperature] mutated: %q", original.Spec.Parameters["temperature"])
	}
	if _, ok := original.Spec.Parameters["new"]; ok {
		t.Error("Parameters mutated: new key leaked into original")
	}
	if original.Status.Phase != "Ready" {
		t.Errorf("Status.Phase mutated: %q", original.Status.Phase)
	}
	if original.Status.Capabilities[0] != "chat" {
		t.Errorf("Status.Capabilities[0] mutated: %q", original.Status.Capabilities[0])
	}
	if original.Status.Conditions[0].Reason != "OK" {
		t.Errorf("Status.Conditions[0].Reason mutated: %q", original.Status.Conditions[0].Reason)
	}
}

// TestModelDeepCopyNilFields verifies DeepCopy handles nil pointer/slice/map fields safely.
func TestModelDeepCopyNilFields(t *testing.T) {
	original := &Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-nil"},
		Spec: ModelSpec{
			ClassName: "c",
			ModelID:   "m",
		},
	}
	copied := original.DeepCopy()
	if copied.Spec.CredentialRef != nil {
		t.Error("CredentialRef should remain nil")
	}
	if copied.Spec.RateLimit != nil {
		t.Error("RateLimit should remain nil")
	}
	if copied.Spec.Capabilities != nil {
		t.Error("Capabilities should remain nil")
	}
	if copied.Spec.Parameters != nil {
		t.Error("Parameters should remain nil")
	}
}

// TestModelListDeepCopy verifies ModelList items are independently copied.
func TestModelListDeepCopy(t *testing.T) {
	original := &ModelList{
		Items: []Model{
			{ObjectMeta: metav1.ObjectMeta{Name: "m1"}, Spec: ModelSpec{ClassName: "c", ModelID: "id1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "m2"}, Spec: ModelSpec{ClassName: "c", ModelID: "id2"}},
		},
	}
	copied := original.DeepCopy()
	copied.Items[0].Spec.ModelID = "mutated"
	if original.Items[0].Spec.ModelID != "id1" {
		t.Errorf("Items[0].Spec.ModelID mutated: %q", original.Items[0].Spec.ModelID)
	}
}

// TestSecretRefDeepCopy verifies SecretRef DeepCopy independence.
func TestSecretRefDeepCopy(t *testing.T) {
	original := &SecretRef{
		ObjectMeta: metav1.ObjectMeta{Name: "sr-1", Namespace: "ns"},
		Spec: SecretRefSpec{
			BackendType: "vault",
			BackendRef:  "secret/data/app",
			Key:         "apiKey",
			Provider:    "legacy-provider",
			Path:        "legacy/path",
			SecretKey:   "legacy-key",
		},
		Status: SecretRefStatus{
			Synced:       true,
			LastSyncTime: "2024-01-01T00:00:00Z",
			Conditions: []metav1.Condition{
				{Type: "Synced", Status: metav1.ConditionTrue, Reason: "OK"},
			},
		},
	}

	copied := original.DeepCopy()
	copied.Spec.BackendType = "env"
	copied.Spec.Key = "other"
	copied.Status.Synced = false
	copied.Status.LastSyncTime = "2025-01-01T00:00:00Z"
	copied.Status.Conditions[0].Reason = "Failed"

	if original.Spec.BackendType != "vault" {
		t.Errorf("BackendType mutated: %q", original.Spec.BackendType)
	}
	if original.Spec.Key != "apiKey" {
		t.Errorf("Key mutated: %q", original.Spec.Key)
	}
	if !original.Status.Synced {
		t.Error("Status.Synced mutated")
	}
	if original.Status.LastSyncTime != "2024-01-01T00:00:00Z" {
		t.Errorf("LastSyncTime mutated: %q", original.Status.LastSyncTime)
	}
	if original.Status.Conditions[0].Reason != "OK" {
		t.Errorf("Conditions[0].Reason mutated: %q", original.Status.Conditions[0].Reason)
	}
}

// TestSecretRefListDeepCopy verifies SecretRefList items are independently copied.
func TestSecretRefListDeepCopy(t *testing.T) {
	original := &SecretRefList{
		Items: []SecretRef{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: SecretRefSpec{BackendType: "vault", Key: "k1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: SecretRefSpec{BackendType: "env", Key: "k2"}},
		},
	}
	copied := original.DeepCopy()
	copied.Items[0].Spec.Key = "mutated"
	if original.Items[0].Spec.Key != "k1" {
		t.Errorf("Items[0].Spec.Key mutated: %q", original.Items[0].Spec.Key)
	}
}
