// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

const BindingKeysEnvironmentVariable = "KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON"

type configuredBindingKey struct {
	Version   int64            `json:"version"`
	Epoch     int64            `json:"epoch"`
	Algorithm string           `json:"algorithm"`
	Domain    string           `json:"domain"`
	Status    BindingKeyStatus `json:"status"`
	Material  string           `json:"material"`
}

// BindingKeyStoreFromJSON parses a secret-backed versioned key ring.
func BindingKeyStoreFromJSON(encoded string) (*StaticBindingKeyStore, error) {
	if encoded == "" {
		return nil, fmt.Errorf("%s is required", BindingKeysEnvironmentVariable)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	var configured []configuredBindingKey
	if err := decoder.Decode(&configured); err != nil {
		return nil, fmt.Errorf("decode %s: %w", BindingKeysEnvironmentVariable, err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return nil, fmt.Errorf("decode %s: %w", BindingKeysEnvironmentVariable, err)
	}
	keys := make([]BindingKey, len(configured))
	var activeEpoch int64
	for index, item := range configured {
		material, err := base64.StdEncoding.DecodeString(item.Material)
		if err != nil {
			return nil, fmt.Errorf("decode runtime binding key version %d material: %w", item.Version, err)
		}
		keys[index] = BindingKey{Version: item.Version, Epoch: item.Epoch, Algorithm: item.Algorithm, Domain: item.Domain, Status: item.Status, Material: material}
		if item.Status == BindingKeyActive {
			activeEpoch = item.Epoch
		}
	}
	return NewStaticBindingKeyStoreAtEpoch(activeEpoch, keys)
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
