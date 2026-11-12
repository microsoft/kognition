// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

func seedMemoryRecord(t *testing.T, h *testHarness, name, ns, memoryRef, content string, derivedFrom []string) {
	t.Helper()
	rec := v1alpha1.MemoryRecord{
		TypeMeta: v1alpha1.TypeMeta{Kind: "MemoryRecord", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.MemoryRecordSpec{
			MemoryRef: v1alpha1.LocalObjectReference{Name: memoryRef},
			Content:   content,
		},
		Status: v1alpha1.MemoryRecordStatus{Phase: v1alpha1.PhaseReady},
	}
	if len(derivedFrom) > 0 {
		rec.Spec.Provenance = &v1alpha1.MemoryProvenance{
			DerivedFromRecordNames: derivedFrom,
		}
	}
	seedResource(t, h, "MemoryRecord", ns, name, rec)
}

func TestMemory_RTBF_SingleRecord(t *testing.T) {
	h := newTestHarness(t)
	seedMemoryRecord(t, h, "rec-1", testNS, "my-memory", "hello world", nil)

	resp := h.doReq(t, http.MethodPost, apiBase+"/memoryrecords/rec-1/rtbf", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var result struct {
		TombstonedRecords []string `json:"tombstonedRecords"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(result.TombstonedRecords) != 1 || result.TombstonedRecords[0] != "rec-1" {
		t.Fatalf("expected [rec-1], got %v", result.TombstonedRecords)
	}
}

func TestMemory_RTBF_CascadeDerived(t *testing.T) {
	h := newTestHarness(t)
	seedMemoryRecord(t, h, "parent", testNS, "my-memory", "parent content", nil)
	seedMemoryRecord(t, h, "child", testNS, "my-memory", "child content", []string{"parent"})
	seedMemoryRecord(t, h, "grandchild", testNS, "my-memory", "grandchild content", []string{"child"})
	// unrelated record in same memory, should NOT be tombstoned
	seedMemoryRecord(t, h, "unrelated", testNS, "my-memory", "unrelated", nil)

	resp := h.doReq(t, http.MethodPost, apiBase+"/memoryrecords/parent/rtbf", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var result struct {
		TombstonedRecords []string `json:"tombstonedRecords"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Should tombstone parent, child, grandchild (BFS cascade)
	tombstoned := map[string]bool{}
	for _, name := range result.TombstonedRecords {
		tombstoned[name] = true
	}
	for _, expected := range []string{"parent", "child", "grandchild"} {
		if !tombstoned[expected] {
			t.Errorf("expected %q to be tombstoned, got %v", expected, result.TombstonedRecords)
		}
	}
	if tombstoned["unrelated"] {
		t.Error("unrelated record should not be tombstoned")
	}
}

func TestMemory_RTBF_NotFound(t *testing.T) {
	h := newTestHarness(t)

	resp := h.doReq(t, http.MethodPost, apiBase+"/memoryrecords/nonexistent/rtbf", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestMemory_RTBF_DifferentMemoryRef(t *testing.T) {
	h := newTestHarness(t)
	seedMemoryRecord(t, h, "rec-a", testNS, "memory-A", "content A", nil)
	seedMemoryRecord(t, h, "rec-b", testNS, "memory-B", "content B", []string{"rec-a"})

	// rec-b references rec-a but is in a different memoryRef → should NOT cascade
	resp := h.doReq(t, http.MethodPost, apiBase+"/memoryrecords/rec-a/rtbf", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var result struct {
		TombstonedRecords []string `json:"tombstonedRecords"`
	}
	json.Unmarshal(resp.Body.Bytes(), &result)

	// Only rec-a should be tombstoned since ListAllMemoryRecords filters by memoryRef
	if len(result.TombstonedRecords) != 1 || result.TombstonedRecords[0] != "rec-a" {
		t.Fatalf("expected only [rec-a], got %v", result.TombstonedRecords)
	}
}
