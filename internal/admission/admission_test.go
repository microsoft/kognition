// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// stubValidating is a validating hook for tests.
type stubValidating struct {
	name     string
	kinds    map[string]bool
	ops      map[Operation]bool
	validate func(ctx context.Context, req *Request) *Response
}

func (h *stubValidating) Name() string { return h.name }
func (h *stubValidating) Handles(kind string, op Operation) bool {
	if h.kinds != nil && !h.kinds[kind] {
		return false
	}
	if h.ops != nil && !h.ops[op] {
		return false
	}
	return true
}
func (h *stubValidating) Validate(ctx context.Context, req *Request) *Response {
	return h.validate(ctx, req)
}

// stubMutating is a mutating hook for tests.
type stubMutating struct {
	name   string
	kinds  map[string]bool
	ops    map[Operation]bool
	mutate func(ctx context.Context, req *Request) *Response
}

func (h *stubMutating) Name() string { return h.name }
func (h *stubMutating) Handles(kind string, op Operation) bool {
	if h.kinds != nil && !h.kinds[kind] {
		return false
	}
	if h.ops != nil && !h.ops[op] {
		return false
	}
	return true
}
func (h *stubMutating) Mutate(ctx context.Context, req *Request) *Response {
	return h.mutate(ctx, req)
}

func TestValidatingHookRejects(t *testing.T) {
	hook := &stubValidating{
		name:  "require-model-ref",
		kinds: map[string]bool{"Agent": true},
		validate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &Response{Allowed: true}
		},
	}

	chain := NewChain(nil, []ValidatingHook{hook})

	// Should reject — no modelRef.
	obj := map[string]any{
		"metadata": map[string]any{"name": "bad-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	data, _ := json.Marshal(obj)

	_, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "bad-agent",
		Object:    data,
	})
	if err == nil {
		t.Fatal("expected admission denial, got nil")
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("expected *DeniedError, got %T: %v", err, err)
	}
	if denied.Hook != "require-model-ref" {
		t.Fatalf("want hook 'require-model-ref', got %q", denied.Hook)
	}
	if denied.Reason != "spec.modelRef is required" {
		t.Fatalf("want reason 'spec.modelRef is required', got %q", denied.Reason)
	}
}

func TestValidatingHookAllows(t *testing.T) {
	hook := &stubValidating{
		name:  "require-model-ref",
		kinds: map[string]bool{"Agent": true},
		validate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &Response{Allowed: true}
		},
	}

	chain := NewChain(nil, []ValidatingHook{hook})

	obj := map[string]any{
		"metadata": map[string]any{"name": "good-agent"},
		"spec":     map[string]any{"modelRef": "models/gpt-4"},
	}
	data, _ := json.Marshal(obj)

	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "good-agent",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if result == nil {
		t.Fatal("expected result object, got nil")
	}
}

func TestMutatingHookInjectsDefault(t *testing.T) {
	hook := &stubMutating{
		name:  "default-timeout",
		kinds: map[string]bool{"Agent": true},
		mutate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = make(map[string]any)
			}
			if _, ok := spec["timeoutSeconds"]; !ok {
				spec["timeoutSeconds"] = float64(30)
				obj["spec"] = spec
				patched, _ := json.Marshal(obj)
				return &Response{Allowed: true, Patch: patched}
			}
			return &Response{Allowed: true}
		},
	}

	chain := NewChain([]MutatingHook{hook}, nil)

	obj := map[string]any{
		"metadata": map[string]any{"name": "no-timeout-agent"},
		"spec":     map[string]any{"modelRef": "models/gpt-4"},
	}
	data, _ := json.Marshal(obj)

	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "no-timeout-agent",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}

	var mutated map[string]any
	if err := json.Unmarshal(result, &mutated); err != nil {
		t.Fatalf("unmarshal mutated: %v", err)
	}
	spec := mutated["spec"].(map[string]any)
	if spec["timeoutSeconds"] != float64(30) {
		t.Fatalf("want timeoutSeconds=30, got %v", spec["timeoutSeconds"])
	}
}

func TestAppendChainPreservesHookOrderAndIgnoresNil(t *testing.T) {
	var calls []string
	mutator := func(name string) MutatingHook {
		return &stubMutating{name: name, mutate: func(_ context.Context, req *Request) *Response {
			calls = append(calls, name)
			return &Response{Allowed: true, Patch: req.Object}
		}}
	}
	validator := func(name string) ValidatingHook {
		return &stubValidating{name: name, validate: func(context.Context, *Request) *Response {
			calls = append(calls, name)
			return &Response{Allowed: true}
		}}
	}

	chain := NewChain([]MutatingHook{mutator("mutate-a")}, []ValidatingHook{validator("validate-a")})
	chain.AppendChain(nil)
	var nilChain *Chain
	nilChain.AppendChain(chain)
	chain.AppendChain(NewChain([]MutatingHook{mutator("mutate-b")}, []ValidatingHook{validator("validate-b")}))

	if _, err := chain.Admit(context.Background(), &Request{Kind: "Agent", Operation: OperationCreate, Object: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	want := []string{"mutate-a", "mutate-b", "validate-a", "validate-b"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestMutatingThenValidatingChain(t *testing.T) {
	// Mutating hook: inject default timeoutSeconds.
	mutator := &stubMutating{
		name:  "default-timeout",
		kinds: map[string]bool{"Agent": true},
		mutate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = make(map[string]any)
			}
			if _, ok := spec["timeoutSeconds"]; !ok {
				spec["timeoutSeconds"] = float64(30)
				obj["spec"] = spec
				patched, _ := json.Marshal(obj)
				return &Response{Allowed: true, Patch: patched}
			}
			return &Response{Allowed: true}
		},
	}

	// Validating hook: require modelRef.
	validator := &stubValidating{
		name:  "require-model-ref",
		kinds: map[string]bool{"Agent": true},
		validate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &Response{Allowed: true}
		},
	}

	chain := NewChain([]MutatingHook{mutator}, []ValidatingHook{validator})

	// Case 1: Agent with modelRef passes; mutator injects timeout.
	obj := map[string]any{
		"metadata": map[string]any{"name": "chain-agent"},
		"spec":     map[string]any{"modelRef": "models/gpt-4"},
	}
	data, _ := json.Marshal(obj)
	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "chain-agent",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("case 1: unexpected denial: %v", err)
	}
	var m map[string]any
	json.Unmarshal(result, &m)
	spec := m["spec"].(map[string]any)
	if spec["timeoutSeconds"] != float64(30) {
		t.Fatalf("case 1: want timeoutSeconds=30, got %v", spec["timeoutSeconds"])
	}
	if spec["modelRef"] != "models/gpt-4" {
		t.Fatalf("case 1: modelRef lost after mutation")
	}

	// Case 2: Agent without modelRef is rejected by validator even after mutation.
	obj2 := map[string]any{
		"metadata": map[string]any{"name": "no-model"},
		"spec":     map[string]any{},
	}
	data2, _ := json.Marshal(obj2)
	_, err = chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "no-model",
		Object:    data2,
	})
	if err == nil {
		t.Fatal("case 2: expected admission denial, got nil")
	}
}

func TestHookKindFiltering(t *testing.T) {
	// Hook only handles "Agent" kind.
	hook := &stubValidating{
		name:  "agent-only",
		kinds: map[string]bool{"Agent": true},
		validate: func(_ context.Context, req *Request) *Response {
			return &Response{Allowed: false, Reason: "always rejects agents"}
		},
	}

	chain := NewChain(nil, []ValidatingHook{hook})

	// Workflow should pass through unaffected.
	obj := map[string]any{"metadata": map[string]any{"name": "w1"}}
	data, _ := json.Marshal(obj)
	_, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Workflow",
		Namespace: "default",
		Name:      "w1",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("workflow should not be rejected by agent-only hook: %v", err)
	}

	// Agent should be rejected.
	_, err = chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "a1",
		Object:    data,
	})
	if err == nil {
		t.Fatal("agent should be rejected by agent-only hook")
	}
}

func TestEmptyChainPassesThrough(t *testing.T) {
	chain := NewChain(nil, nil)

	obj := map[string]any{"metadata": map[string]any{"name": "test"}}
	data, _ := json.Marshal(obj)

	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "test",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("empty chain should not reject: %v", err)
	}
	if string(result) != string(data) {
		t.Fatalf("empty chain should return original object")
	}
}

func TestMutatingHookDenies(t *testing.T) {
	hook := &stubMutating{
		name: "deny-all",
		mutate: func(_ context.Context, req *Request) *Response {
			return &Response{Allowed: false, Reason: "not allowed"}
		},
	}

	chain := NewChain([]MutatingHook{hook}, nil)
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "x"}})

	_, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err == nil {
		t.Fatal("expected denial from mutating hook")
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("expected *DeniedError, got %T", err)
	}
	if denied.Hook != "deny-all" {
		t.Fatalf("want hook 'deny-all', got %q", denied.Hook)
	}
}

func TestNilResponseIsNoOp(t *testing.T) {
	// Hook returns nil → treated as no-op (allowed, no mutation).
	mutator := &stubMutating{
		name:   "nil-response",
		mutate: func(_ context.Context, req *Request) *Response { return nil },
	}
	validator := &stubValidating{
		name:     "nil-response",
		validate: func(_ context.Context, req *Request) *Response { return nil },
	}

	chain := NewChain([]MutatingHook{mutator}, []ValidatingHook{validator})
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "x"}})

	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("nil response should not reject: %v", err)
	}
	if string(result) != string(data) {
		t.Fatal("nil response should leave object unchanged")
	}
}

func TestRegisterHooks(t *testing.T) {
	chain := NewChain(nil, nil)

	mutator := &stubMutating{
		name:   "m1",
		mutate: func(_ context.Context, req *Request) *Response { return nil },
	}
	validator := &stubValidating{
		name:     "v1",
		validate: func(_ context.Context, req *Request) *Response { return nil },
	}

	chain.RegisterMutating(mutator)
	chain.RegisterValidating(validator)

	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "x"}})
	_, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("registered hooks should not reject: %v", err)
	}
}

func TestDeniedErrorMessage(t *testing.T) {
	err := &DeniedError{Hook: "my-hook", Reason: "bad input"}
	want := "admission denied by my-hook: bad input"
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err.Error())
	}
}

func TestHookOperationFiltering(t *testing.T) {
	// Validating hook only handles UPDATE operations.
	hook := &stubValidating{
		name: "update-only",
		ops:  map[Operation]bool{OperationUpdate: true},
		validate: func(_ context.Context, req *Request) *Response {
			return &Response{Allowed: false, Reason: "always rejects"}
		},
	}

	chain := NewChain(nil, []ValidatingHook{hook})
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "x"}})

	// CREATE should pass through since hook only handles UPDATE.
	_, err := chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("CREATE should not be rejected by update-only hook: %v", err)
	}

	// UPDATE should be rejected.
	_, err = chain.Admit(context.Background(), &Request{
		Operation: OperationUpdate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err == nil {
		t.Fatal("UPDATE should be rejected by update-only hook")
	}
}

func TestMutatingOperationFiltering(t *testing.T) {
	// Mutating hook only handles CREATE operations.
	hook := &stubMutating{
		name: "create-only-mutator",
		ops:  map[Operation]bool{OperationCreate: true},
		mutate: func(_ context.Context, req *Request) *Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			obj["injected"] = true
			patched, _ := json.Marshal(obj)
			return &Response{Allowed: true, Patch: patched}
		},
	}

	chain := NewChain([]MutatingHook{hook}, nil)
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "x"}})

	// UPDATE should pass through without mutation.
	result, err := chain.Admit(context.Background(), &Request{
		Operation: OperationUpdate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("UPDATE should not be rejected: %v", err)
	}
	if string(result) != string(data) {
		t.Fatal("UPDATE should not be mutated by create-only hook")
	}

	// CREATE should be mutated.
	result, err = chain.Admit(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "x",
		Object:    data,
	})
	if err != nil {
		t.Fatalf("CREATE should not be rejected: %v", err)
	}
	var obj map[string]any
	json.Unmarshal(result, &obj)
	if obj["injected"] != true {
		t.Fatal("CREATE should be mutated by create-only hook")
	}
}
