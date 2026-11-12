// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestStoreAdapterInvocationProfileCRUD(t *testing.T) {
	ctx := context.Background()
	adapter := NewStoreAdapter(newFakeClient(t))
	maximum := int64(512)
	profile := api.ModelInvocationProfile{
		TypeMeta:   api.TypeMeta{Kind: "ModelInvocationProfile", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "profile-v1", Namespace: "team-a", UID: "profile-uid"},
		Spec:       api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Create(ctx, "ModelInvocationProfile", "team-a", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	data, err = adapter.Get(ctx, "ModelInvocationProfile", "team-a", profile.Name)
	if err != nil || json.Unmarshal(data, &profile) != nil || profile.Spec.Defaults.MaxCompletionTokens == nil || *profile.Spec.Defaults.MaxCompletionTokens != 512 {
		t.Fatalf("profile roundtrip=%s error=%v", data, err)
	}
	profile.Labels = map[string]string{"reviewed": "true"}
	data, err = json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Update(ctx, "ModelInvocationProfile", "team-a", profile.Name, data, profile.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	profiles, err := adapter.List(ctx, "ModelInvocationProfile", store.ListOptions{Namespace: "team-a"})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("list=%d error=%v", len(profiles), err)
	}
	if err := json.Unmarshal(profiles[0], &profile); err != nil || profile.Labels["reviewed"] != "true" {
		t.Fatalf("profile metadata update lost: %+v, %v", profile, err)
	}
	if err := adapter.Delete(ctx, "ModelInvocationProfile", "team-a", profile.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Get(ctx, "ModelInvocationProfile", "team-a", profile.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted profile error=%v", err)
	}
}
