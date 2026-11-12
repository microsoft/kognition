// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package recovery

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

const MaxPayloadBytes = 512 << 10

var ErrUnavailable = errors.New("runtime recovery unavailable")

type Binding struct {
	Workspace           string `json:"workspace"`
	AgentUID            string `json:"agentUID"`
	InstanceUID         string `json:"instanceUID"`
	SessionID           string `json:"sessionID"`
	Generation          int32  `json:"generation"`
	Runtime             string `json:"runtime"`
	ConfigurationDigest string `json:"configurationDigest"`
}

type WorkspaceKeys struct {
	Current string            `json:"current"`
	Keys    map[string]string `json:"keys"`
}

type Cipher struct{ workspaces map[string]WorkspaceKeys }

type envelope struct {
	Version    int       `json:"version"`
	KeyID      string    `json:"keyID"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
}

func NewCipher(configuration []byte) (*Cipher, error) {
	if len(configuration) == 0 || len(configuration) > 256<<10 {
		return nil, ErrUnavailable
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(configuration)
	if err != nil {
		return nil, ErrUnavailable
	}
	var workspaces map[string]WorkspaceKeys
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&workspaces) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(workspaces) == 0 || len(workspaces) > 200 {
		return nil, ErrUnavailable
	}
	for namespace, ring := range workspaces {
		if namespace == "" || namespace == "*" || ring.Current == "" || len(ring.Keys) == 0 || len(ring.Keys) > 16 || ring.Keys[ring.Current] == "" {
			return nil, ErrUnavailable
		}
		for keyID, encoded := range ring.Keys {
			key, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if keyID == "" || len(keyID) > 128 || err != nil || len(key) != 32 {
				return nil, ErrUnavailable
			}
		}
	}
	return &Cipher{workspaces: workspaces}, nil
}

func associatedData(binding Binding, keyID string, expiry time.Time) ([]byte, error) {
	if binding.Workspace == "" || binding.AgentUID == "" || binding.InstanceUID == "" || binding.SessionID == "" || binding.Generation < 0 || binding.Runtime == "" || len(binding.ConfigurationDigest) != 64 || keyID == "" || expiry.IsZero() {
		return nil, ErrUnavailable
	}
	return json.Marshal(struct {
		Domain    string    `json:"domain"`
		Binding   Binding   `json:"binding"`
		KeyID     string    `json:"keyID"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{"kognition.dev/runtime-checkpoint/v1", binding, keyID, expiry.UTC()})
}

func (crypto *Cipher) aead(namespace, keyID string) (cipher.AEAD, error) {
	if crypto == nil {
		return nil, ErrUnavailable
	}
	encoded := crypto.workspaces[namespace].Keys[keyID]
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrUnavailable
	}
	return cipher.NewGCM(block)
}

func (crypto *Cipher) Seal(binding Binding, plaintext []byte, expiry, now time.Time) ([]byte, error) {
	if crypto == nil || len(plaintext) == 0 || len(plaintext) > MaxPayloadBytes || !expiry.After(now) || expiry.Sub(now) > 30*24*time.Hour {
		return nil, ErrUnavailable
	}
	keyID := crypto.workspaces[binding.Workspace].Current
	data, err := associatedData(binding, keyID, expiry)
	if err != nil {
		return nil, err
	}
	aead, err := crypto.aead(binding.Workspace, keyID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrUnavailable
	}
	return json.Marshal(envelope{Version: 1, KeyID: keyID, ExpiresAt: expiry.UTC(), Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, data)})
}

func (crypto *Cipher) Open(binding Binding, encoded []byte, now time.Time) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > 768<<10 {
		return nil, ErrUnavailable
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(encoded)
	if err != nil {
		return nil, ErrUnavailable
	}
	var sealed envelope
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&sealed) != nil || decoder.Decode(&struct{}{}) != io.EOF || sealed.Version != 1 || !sealed.ExpiresAt.After(now) {
		return nil, ErrUnavailable
	}
	data, err := associatedData(binding, sealed.KeyID, sealed.ExpiresAt)
	if err != nil {
		return nil, err
	}
	aead, err := crypto.aead(binding.Workspace, sealed.KeyID)
	if err != nil || len(sealed.Nonce) != aead.NonceSize() {
		return nil, ErrUnavailable
	}
	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, data)
	if err != nil || len(plaintext) > MaxPayloadBytes {
		return nil, ErrUnavailable
	}
	return plaintext, nil
}
