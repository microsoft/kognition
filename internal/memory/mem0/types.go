// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mem0

import (
	"time"

	"github.com/kognition-ai/kognition/internal/memory"
)

// --- Mem0 API request/response types ---

// mem0Message represents a chat message in Mem0's format.
type mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// mem0AddRequest is the payload for POST /v1/memories/.
type mem0AddRequest struct {
	Messages  []mem0Message     `json:"messages"`
	UserID    string            `json:"user_id"`
	MemoryID  string            `json:"memory_id,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	OrgID     string            `json:"org_id,omitempty"`
	ProjectID string            `json:"project_id,omitempty"`
}

// mem0AddResult is a single result entry in the add response.
type mem0AddResult struct {
	ID     string `json:"id"`
	Memory string `json:"memory"`
	Event  string `json:"event"`
}

// mem0AddResponse is the response from POST /v1/memories/.
type mem0AddResponse struct {
	Results []mem0AddResult `json:"results"`
}

// mem0Memory represents a single memory object returned by Mem0.
type mem0Memory struct {
	ID        string            `json:"id"`
	Memory    string            `json:"memory"`
	UserID    string            `json:"user_id"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

// toRecord converts a Mem0 memory to our internal Record type.
func (m mem0Memory) toRecord() memory.Record {
	ts, _ := time.Parse(time.RFC3339, m.CreatedAt)
	return memory.Record{
		ID:        m.ID,
		Content:   m.Memory,
		Metadata:  m.Metadata,
		Timestamp: ts,
	}
}

// mem0ListResponse is the response from GET /v1/memories/.
type mem0ListResponse struct {
	Count   int          `json:"count"`
	Next    string       `json:"next"`
	Results []mem0Memory `json:"results"`
}
