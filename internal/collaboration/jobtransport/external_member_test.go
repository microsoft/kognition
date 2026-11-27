// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobtransport

import (
	"context"
	"errors"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
)

func TestExternalAgentMemberIsNeverRunAsAnAgentJob(t *testing.T) {
	st := newFakeStore()
	run := testRun()
	run.Spec.Members = append(run.Spec.Members, api.CollaborationMember{Name: "ext", ExternalAgentRef: &api.LocalObjectReference{Name: "partner"}})
	tr, err := New(Config{Store: st, Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Admit(context.Background(), startMsg("ext")); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("external member admitted by the AgentJob transport: %v", err)
	}
	if err := tr.Send(context.Background(), startMsg("ext")); err == nil {
		t.Fatal("external member was sent to by the AgentJob transport")
	}
	if len(st.jobs) != 0 {
		t.Fatalf("an AgentJob was created for an external member: %d", len(st.jobs))
	}
}
