// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/sigstore/sigstore/pkg/signature"
)

// stubStore implements store.Store for signature validator tests.
type stubStore struct {
	data   map[string][]byte
	getErr error
}

func (s *stubStore) Ping(_ context.Context) error { return nil }
func (s *stubStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	key := kind + "/" + namespace + "/" + name
	if d, ok := s.data[key]; ok {
		return d, nil
	}
	return nil, store.ErrNotFound
}
func (s *stubStore) List(_ context.Context, _ string, _ store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (s *stubStore) Create(_ context.Context, _, _, _ string, _ []byte) error { return nil }
func (s *stubStore) Update(_ context.Context, _, _, _ string, _ []byte, _ int64) error {
	return nil
}
func (s *stubStore) Delete(_ context.Context, _, _, _ string) error { return nil }
func (s *stubStore) Watch(_ context.Context, _ string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (s *stubStore) CreateRevision(_ context.Context, _, _, _ string, _ int64, _, _ []byte) error {
	return nil
}
func (s *stubStore) GetRevision(_ context.Context, _, _, _ string, _ int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (s *stubStore) ListRevisions(_ context.Context, _, _, _ string) ([]store.Revision, error) {
	return nil, nil
}
func (s *stubStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return nil
}
func (s *stubStore) ListEvents(_ context.Context, _, _, _ string) ([]store.InstanceEvent, error) {
	return nil, nil
}

func workspaceBytes(requireSigned bool) []byte {
	ws := v1alpha1.Workspace{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workspace"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec:       v1alpha1.WorkspaceSpec{RequireSignedArtifacts: requireSigned},
	}
	b, _ := json.Marshal(ws)
	return b
}

// TC-0059: Unsigned Tool rejected when workspace requires signatures.
func TestSignatureValidator_RejectsUnsignedTool(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Workspace/default/default": workspaceBytes(true),
	}}
	v := NewSignatureValidator(st)

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec:       v1alpha1.ToolSpec{ClassName: "process-networked"},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected unsigned tool to be rejected")
	}
}

// TC-0060: Signed Tool accepted when workspace requires signatures.
func TestSignatureValidator_AcceptsSignedTool(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Workspace/default/default": workspaceBytes(true),
	}}

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec:       v1alpha1.ToolSpec{ClassName: "process-networked"},
	}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	data = signArtifactForTest(t, "Tool", "default", "web-search", data)
	v := NewSignatureValidator(st)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected signed tool to be accepted, got: %s", resp.Reason)
	}
}

func signArtifactForTest(test *testing.T, kind, namespace, name string, object []byte) []byte {
	test.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		test.Fatal(err)
	}
	configuration, err := json.Marshal(map[string]map[string]string{namespace: {"release": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKey}))}})
	if err != nil {
		test.Fatal(err)
	}
	test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", string(configuration))
	payload, err := ArtifactSignaturePayload(kind, namespace, name, object)
	if err != nil {
		test.Fatal(err)
	}
	signer, err := signature.LoadSigner(privateKey, crypto.SHA256)
	if err != nil {
		test.Fatal(err)
	}
	signed, err := signer.SignMessage(bytes.NewReader(payload))
	if err != nil {
		test.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"version": 1, "keyID": "release", "signature": base64.StdEncoding.EncodeToString(signed)})
	if err != nil {
		test.Fatal(err)
	}
	var resource map[string]any
	if err := json.Unmarshal(object, &resource); err != nil {
		test.Fatal(err)
	}
	resource["spec"].(map[string]any)["signature"] = string(envelope)
	result, err := json.Marshal(resource)
	if err != nil {
		test.Fatal(err)
	}
	return result
}

func TestSignatureValidator_VerifiesExactPinnedArtifact(test *testing.T) {
	for _, kind := range []string{"Tool", "Model"} {
		test.Run(kind, func(test *testing.T) {
			for _, scenario := range []string{"valid", "forged", "changed spec", "changed name", "changed namespace", "untrusted key", "missing trust", "malformed trust", "unknown envelope", "cancelled"} {
				test.Run(scenario, func(test *testing.T) {
					unsigned := []byte(`{"metadata":{"name":"artifact","namespace":"default"},"spec":{"endpoint":"https://provider.example.test","signature":""}}`)
					object := signArtifactForTest(test, kind, "default", "artifact", unsigned)
					ctx := context.Background()
					request := &Request{Operation: OperationCreate, Kind: kind, Namespace: "default", Name: "artifact", Object: object}
					var resource map[string]any
					if err := json.Unmarshal(object, &resource); err != nil {
						test.Fatal(err)
					}
					spec := resource["spec"].(map[string]any)
					switch scenario {
					case "forged":
						spec["signature"] = "not-a-cryptographic-signature"
					case "changed spec":
						spec["endpoint"] = "https://different.example.test"
					case "changed name":
						request.Name = "other"
					case "changed namespace":
						request.Namespace = "other"
					case "untrusted key":
						spec["signature"] = strings.Replace(spec["signature"].(string), "release", "unknown", 1)
					case "missing trust":
						test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", "")
					case "malformed trust":
						test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", `{"default":{"release":"invalid"}}`)
					case "unknown envelope":
						spec["signature"] = strings.TrimSuffix(spec["signature"].(string), "}") + `,"key":"attacker-supplied"}`
					case "cancelled":
						cancelled, cancel := context.WithCancel(ctx)
						cancel()
						ctx = cancelled
					}
					var err error
					request.Object, err = json.Marshal(resource)
					if err != nil {
						test.Fatal(err)
					}
					storage := &stubStore{data: map[string][]byte{
						"Workspace/default/default": workspaceBytes(true),
						"Workspace/other/other":     workspaceBytes(true),
					}}
					response := NewSignatureValidator(storage).Validate(ctx, request)
					if response.Allowed != (scenario == "valid") {
						test.Fatalf("unexpected signature decision: %+v", response)
					}
				})
			}
		})
	}
}

func TestSignatureValidator_RereadsOperatorTrustAndFailsClosed(test *testing.T) {
	unsigned := []byte(`{"metadata":{"name":"artifact","namespace":"default"},"spec":{"endpoint":"https://provider.example.test"}}`)
	object := signArtifactForTest(test, "Tool", "default", "artifact", unsigned)
	configuration := os.Getenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON")
	file := filepath.Join(test.TempDir(), "public-keys.json")
	if err := os.WriteFile(file, []byte(configuration), 0600); err != nil {
		test.Fatal(err)
	}
	test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", "")
	test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_FILE", file)
	storage := &stubStore{data: map[string][]byte{"Workspace/default/default": workspaceBytes(true)}}
	validator := NewSignatureValidator(storage)
	request := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "artifact", Object: object}
	if response := validator.Validate(context.Background(), request); !response.Allowed {
		test.Fatalf("mounted trusted key was rejected: %+v", response)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(configuration, "release", "rotated", 1)), 0600); err != nil {
		test.Fatal(err)
	}
	if response := validator.Validate(context.Background(), request); response.Allowed {
		test.Fatal("removed signer retained cached authority")
	}
	if err := os.Remove(file); err != nil {
		test.Fatal(err)
	}
	if response := validator.Validate(context.Background(), request); response.Allowed {
		test.Fatal("unavailable key file retained signer authority")
	}
}

func TestProductionAdmissionVerifiesPinnedSignatures(test *testing.T) {
	tool := v1alpha1.Tool{TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"}, ObjectMeta: v1alpha1.ObjectMeta{Name: "artifact", Namespace: "default"}, Spec: v1alpha1.ToolSpec{ClassName: "process-networked", Protocol: v1alpha1.ToolProtocolMCP, Endpoint: "https://provider.example.test"}}
	unsigned, err := json.Marshal(tool)
	if err != nil {
		test.Fatal(err)
	}
	signed := signArtifactForTest(test, "Tool", "default", "artifact", unsigned)
	storage := &stubStore{data: map[string][]byte{"Workspace/default/default": workspaceBytes(true)}}
	mutating, validating := ProductionHooks(storage, opa.NewEngine())
	chain := NewChain(mutating, validating)
	request := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "artifact", Object: signed}
	if _, err := chain.Admit(context.Background(), request); err != nil {
		test.Fatalf("production chain rejected a trusted artifact: %v", err)
	}
	tool.Spec.Signature = "not-a-signature"
	request.Object, err = json.Marshal(tool)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := chain.Admit(context.Background(), request); err == nil {
		test.Fatal("production chain accepted an invalid signature")
	}
}

// Tool without policy enforcement should pass without signature.
func TestSignatureValidator_AllowsWhenPolicyDisabled(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Workspace/default/default": workspaceBytes(false),
	}}
	v := NewSignatureValidator(st)

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec:       v1alpha1.ToolSpec{ClassName: "process-networked"},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected tool to be accepted when policy disabled, got: %s", resp.Reason)
	}
}

func TestSignatureValidator_WorkspacePolicyLookup(t *testing.T) {
	tests := []struct {
		name       string
		store      *stubStore
		wantAllow  bool
		wantReason string
	}{
		{
			name:      "missing workspace does not enforce policy",
			store:     &stubStore{data: map[string][]byte{}},
			wantAllow: true,
		},
		{
			name:       "malformed workspace fails closed",
			store:      &stubStore{data: map[string][]byte{"Workspace/default/default": []byte(`{`)}},
			wantReason: "failed to look up workspace policy: unmarshal workspace:",
		},
		{
			name:       "store failure fails closed",
			store:      &stubStore{getErr: errors.New("store unavailable")},
			wantReason: "failed to look up workspace policy: store unavailable",
		},
	}

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec:       v1alpha1.ToolSpec{ClassName: "process-networked"},
	}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal Tool: %v", err)
	}
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := NewSignatureValidator(test.store).Validate(context.Background(), req)
			if resp.Allowed != test.wantAllow {
				t.Errorf("Allowed = %t, want %t (reason: %s)", resp.Allowed, test.wantAllow, resp.Reason)
			}
			if test.wantReason == "" && resp.Reason != "" {
				t.Errorf("Reason = %q, want empty reason", resp.Reason)
			}
			if test.wantReason != "" && !strings.Contains(resp.Reason, test.wantReason) {
				t.Errorf("Reason = %q, want substring %q", resp.Reason, test.wantReason)
			}
		})
	}
}

func TestSignatureValidator_RejectsMalformedArtifact(t *testing.T) {
	for _, kind := range []string{"Tool", "Model"} {
		t.Run(kind, func(t *testing.T) {
			req := &Request{Operation: OperationCreate, Kind: kind, Namespace: "default", Name: "artifact", Object: []byte(`{`)}
			resp := NewSignatureValidator(&stubStore{}).Validate(context.Background(), req)

			if resp.Allowed {
				t.Fatal("Validate() allowed malformed artifact")
			}
			if !strings.Contains(resp.Reason, "invalid "+kind+" JSON:") {
				t.Errorf("Reason = %q, want invalid %s JSON error", resp.Reason, kind)
			}
		})
	}
}

func TestSignatureValidator_HandlesSignedArtifactWrites(t *testing.T) {
	validator := NewSignatureValidator(&stubStore{})
	tests := []struct {
		kind string
		op   Operation
		want bool
	}{
		{"Tool", OperationCreate, true},
		{"Tool", OperationUpdate, true},
		{"Model", OperationCreate, true},
		{"Model", OperationUpdate, true},
		{"Agent", OperationCreate, false},
		{"Tool", Operation("DELETE"), false},
	}

	for _, test := range tests {
		if got := validator.Handles(test.kind, test.op); got != test.want {
			t.Errorf("Handles(%q, %q) = %t, want %t", test.kind, test.op, got, test.want)
		}
	}
}

// Unsigned Model rejected when workspace requires signatures.
func TestSignatureValidator_RejectsUnsignedModel(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Workspace/default/default": workspaceBytes(true),
	}}
	v := NewSignatureValidator(st)

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "gpt-4", Namespace: "default"},
		Spec:       v1alpha1.ModelSpec{Provider: "openai", ModelID: "gpt-4"},
	}
	data, _ := json.Marshal(model)
	req := &Request{Operation: OperationCreate, Kind: "Model", Namespace: "default", Name: "gpt-4", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected unsigned model to be rejected")
	}
}
