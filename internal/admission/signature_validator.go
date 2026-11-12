// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/sigstore/sigstore/pkg/signature"
)

const artifactSigningKeysEnvironment = "KOGNITION_ARTIFACT_SIGNING_KEYS_JSON"

type artifactSignatureEnvelope struct {
	Version   int    `json:"version"`
	KeyID     string `json:"keyID"`
	Signature string `json:"signature"`
}

// SignatureValidator validates that Tool and Model resources carry a valid
// supply-chain signature when the owning Workspace requires signed artifacts.
type SignatureValidator struct {
	store      store.Store
	keys       map[string]map[string]signature.Verifier
	trustError error
	keyFile    string
}

// NewSignatureValidator creates a SignatureValidator backed by the given store.
func NewSignatureValidator(s store.Store) *SignatureValidator {
	keys, err := loadArtifactSigningKeys(os.Getenv(artifactSigningKeysEnvironment))
	file := os.Getenv("KOGNITION_ARTIFACT_SIGNING_KEYS_FILE")
	if file != "" && strings.TrimSpace(os.Getenv(artifactSigningKeysEnvironment)) != "" {
		err = errors.New("artifact trust must select exactly one configuration source")
	}
	return &SignatureValidator{store: s, keys: keys, trustError: err, keyFile: file}
}

func (v *SignatureValidator) Name() string { return "signature-validator" }

func (v *SignatureValidator) Handles(kind string, op Operation) bool {
	return (kind == "Tool" || kind == "Model") && (op == OperationCreate || op == OperationUpdate)
}

func (v *SignatureValidator) Validate(ctx context.Context, req *Request) *Response {
	if ctx.Err() != nil {
		return &Response{Allowed: false, Reason: "artifact verification cancelled"}
	}
	// Determine signature from the resource being admitted.
	signature, err := extractSignature(req.Kind, req.Object)
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid %s JSON: %v", req.Kind, err)}
	}

	// Look up workspace policy.
	required, err := v.workspaceRequiresSignature(ctx, req.Namespace)
	if err != nil {
		// If workspace not found, policy is not enforced.
		if errors.Is(err, store.ErrNotFound) {
			return &Response{Allowed: true}
		}
		return &Response{Allowed: false, Reason: fmt.Sprintf("failed to look up workspace policy: %v", err)}
	}

	if !required {
		return &Response{Allowed: true}
	}

	// Policy is enforced — signature must be present and well-formed.
	if signature == "" {
		return &Response{Allowed: false, Reason: fmt.Sprintf("%s %s/%s rejected: workspace requires a valid supply-chain signature", req.Kind, req.Namespace, req.Name)}
	}
	if err := v.VerifySignedArtifact(ctx, req); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}

func (v *SignatureValidator) VerifySignedArtifact(ctx context.Context, req *Request) error {
	encoded, err := extractSignature(req.Kind, req.Object)
	if err != nil || encoded == "" || ctx.Err() != nil {
		return errors.New("artifact signature verification failed")
	}
	keys := v.keys
	if v.keyFile != "" && v.trustError == nil {
		var err error
		keys, err = readArtifactSigningKeys(v.keyFile)
		if err != nil {
			return errors.New("artifact signer trust is unavailable")
		}
	}
	if v.trustError != nil || len(keys[req.Namespace]) == 0 {
		return errors.New("artifact signer trust is not configured for this workspace")
	}
	if err := v.verifyArtifact(ctx, req, encoded, keys[req.Namespace]); err != nil {
		return errors.New("artifact signature verification failed")
	}
	return nil
}

func readArtifactSigningKeys(path string) (map[string]map[string]signature.Verifier, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	configuration, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil {
		return nil, err
	}
	return loadArtifactSigningKeys(string(configuration))
}

func loadArtifactSigningKeys(configuration string) (map[string]map[string]signature.Verifier, error) {
	keys := map[string]map[string]signature.Verifier{}
	if strings.TrimSpace(configuration) == "" {
		return keys, nil
	}
	if len(configuration) > 256<<10 {
		return nil, errors.New("artifact signer configuration exceeds size limit")
	}
	canonical, err := runtimeapproval.CanonicalizeJSON([]byte(configuration))
	if err != nil {
		return nil, errors.New("invalid artifact signer configuration")
	}
	var configured map[string]map[string]string
	if err := json.Unmarshal(canonical, &configured); err != nil || len(configured) == 0 || len(configured) > 200 {
		return nil, errors.New("invalid artifact signer configuration")
	}
	for namespace, signers := range configured {
		if namespace == "" || namespace == "*" || len(signers) == 0 || len(signers) > 16 {
			return nil, errors.New("invalid artifact signer scope")
		}
		keys[namespace] = map[string]signature.Verifier{}
		for name, encoded := range signers {
			if name == "" || len(name) > 128 {
				return nil, errors.New("invalid artifact signer identity")
			}
			block, remaining := pem.Decode([]byte(encoded))
			if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 || strings.TrimSpace(string(remaining)) != "" {
				return nil, errors.New("invalid artifact signer public key")
			}
			publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, errors.New("invalid artifact signer public key")
			}
			switch key := publicKey.(type) {
			case *rsa.PublicKey:
				if key.N.BitLen() < 2048 {
					return nil, errors.New("artifact signer RSA key is too weak")
				}
			case *ecdsa.PublicKey, ed25519.PublicKey:
			default:
				return nil, errors.New("unsupported artifact signer key")
			}
			verifier, err := signature.LoadVerifier(publicKey, crypto.SHA256)
			if err != nil {
				return nil, errors.New("unsupported artifact signer key")
			}
			keys[namespace][name] = verifier
		}
	}
	return keys, nil
}

func ArtifactSignaturePayload(kind, namespace, name string, object []byte) ([]byte, error) {
	if kind != "Tool" && kind != "Model" || namespace == "" || name == "" || len(object) > 4<<20 {
		return nil, errors.New("invalid artifact signing target")
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(object)
	if err != nil {
		return nil, errors.New("invalid artifact signing document")
	}
	var resource struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(canonical, &resource); err != nil || resource.Spec == nil {
		return nil, errors.New("artifact specification is required")
	}
	delete(resource.Spec, "signature")
	payload, err := json.Marshal(struct {
		Domain    string                     `json:"domain"`
		Kind      string                     `json:"kind"`
		Namespace string                     `json:"namespace"`
		Name      string                     `json:"name"`
		Spec      map[string]json.RawMessage `json:"spec"`
	}{Domain: "kognition.dev/artifact-signature/v1", Kind: kind, Namespace: namespace, Name: name, Spec: resource.Spec})
	if err != nil {
		return nil, err
	}
	return runtimeapproval.CanonicalizeJSON(payload)
}

func (v *SignatureValidator) verifyArtifact(ctx context.Context, req *Request, encoded string, keys map[string]signature.Verifier) error {
	if len(encoded) > 16<<10 {
		return errors.New("artifact signature exceeds size limit")
	}
	canonical, err := runtimeapproval.CanonicalizeJSON([]byte(encoded))
	if err != nil {
		return errors.New("invalid artifact signature envelope")
	}
	var envelope artifactSignatureEnvelope
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Version != 1 || envelope.KeyID == "" {
		return errors.New("invalid artifact signature envelope")
	}
	verifier := keys[envelope.KeyID]
	if verifier == nil {
		return errors.New("untrusted artifact signer")
	}
	signed, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signed) == 0 {
		return errors.New("invalid detached artifact signature")
	}
	payload, err := ArtifactSignaturePayload(req.Kind, req.Namespace, req.Name, req.Object)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return verifier.VerifySignature(bytes.NewReader(signed), bytes.NewReader(payload))
}

// extractSignature returns the Signature field from a Tool or Model resource.
func extractSignature(kind string, data []byte) (string, error) {
	switch kind {
	case "Tool":
		var t v1alpha1.Tool
		if err := json.Unmarshal(data, &t); err != nil {
			return "", err
		}
		return t.Spec.Signature, nil
	case "Model":
		var m v1alpha1.Model
		if err := json.Unmarshal(data, &m); err != nil {
			return "", err
		}
		return m.Spec.Signature, nil
	default:
		return "", fmt.Errorf("unsupported kind: %s", kind)
	}
}

// workspaceRequiresSignature looks up the Workspace for the given namespace
// and returns whether signed artifacts are required.
func (v *SignatureValidator) workspaceRequiresSignature(ctx context.Context, namespace string) (bool, error) {
	data, err := v.store.Get(ctx, "Workspace", namespace, namespace)
	if err != nil {
		return false, err
	}
	var ws v1alpha1.Workspace
	if err := json.Unmarshal(data, &ws); err != nil {
		return false, fmt.Errorf("unmarshal workspace: %w", err)
	}
	return ws.Spec.RequireSignedArtifacts, nil
}
