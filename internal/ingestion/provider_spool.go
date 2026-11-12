// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	ProviderSpoolMaxBytes  = 64 << 20
	ProviderSpoolMaxChunks = 100_000
	providerSpoolMaxLine   = 4 << 20
)

type ProviderSpoolOptions struct {
	Scope             *protocol.KnowledgeScope
	OperationID       string
	ExpectedDimension int
	MaxBytes          int64
	MaxChunks         int
}

type PreparedProviderIndex struct {
	Batches         []*protocol.KnowledgeIndexBatchRequest
	SourceDocuments int64
}

type providerSpoolRecord struct {
	Type            string    `json:"type"`
	DocumentID      string    `json:"documentId,omitempty"`
	ChunkID         string    `json:"chunkId,omitempty"`
	Content         string    `json:"content,omitempty"`
	SourceURI       string    `json:"sourceUri,omitempty"`
	Location        string    `json:"location,omitempty"`
	Vector          []float64 `json:"vector,omitempty"`
	SourceDocuments *int64    `json:"sourceDocuments,omitempty"`
	IndexedChunks   *int64    `json:"indexedChunks,omitempty"`
	SourceDigest    string    `json:"sourceDigest,omitempty"`
}

func ReadProviderSpool(reader io.Reader, options ProviderSpoolOptions) (*PreparedProviderIndex, error) {
	if reader == nil || options.Scope == nil || options.ExpectedDimension < 1 {
		return nil, errors.New("provider spool requires a reader, trusted scope and expected dimension")
	}
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = ProviderSpoolMaxBytes
	}
	maxChunks := options.MaxChunks
	if maxChunks == 0 {
		maxChunks = ProviderSpoolMaxChunks
	}
	if maxBytes < 1 || maxChunks < 1 {
		return nil, errors.New("provider spool limits must be positive")
	}

	limited := &io.LimitedReader{R: reader, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), providerSpoolMaxLine)
	chunks := make([]*protocol.KnowledgeIndexChunk, 0)
	identities := make(map[[2]string]struct{})
	var summary *providerSpoolRecord
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return nil, fmt.Errorf("provider spool line %d is empty", lineNumber)
		}
		if summary != nil {
			return nil, errors.New("provider spool summary must be the final record")
		}
		record, err := decodeProviderSpoolRecord(line)
		if err != nil {
			return nil, fmt.Errorf("decode provider spool line %d: %w", lineNumber, err)
		}
		switch record.Type {
		case "chunk":
			if record.SourceDocuments != nil || record.IndexedChunks != nil || record.SourceDigest != "" {
				return nil, fmt.Errorf("provider spool line %d mixes chunk and summary fields", lineNumber)
			}
			if len(chunks) >= maxChunks {
				return nil, fmt.Errorf("provider spool exceeds %d chunks", maxChunks)
			}
			chunk := &protocol.KnowledgeIndexChunk{
				DocumentId: record.DocumentID, ChunkId: record.ChunkID, Content: record.Content,
				SourceUri: record.SourceURI, Location: record.Location, Vector: record.Vector,
			}
			if len(chunk.Vector) != options.ExpectedDimension {
				return nil, fmt.Errorf("provider spool line %d vector dimension does not match trusted configuration", lineNumber)
			}
			identity := [2]string{chunk.DocumentId, chunk.ChunkId}
			if _, found := identities[identity]; found {
				return nil, fmt.Errorf("provider spool line %d duplicates a chunk identity", lineNumber)
			}
			identities[identity] = struct{}{}
			chunks = append(chunks, chunk)
		case "summary":
			if record.DocumentID != "" || record.ChunkID != "" || record.Content != "" || record.SourceURI != "" || record.Location != "" || record.Vector != nil ||
				record.SourceDocuments == nil || record.IndexedChunks == nil {
				return nil, fmt.Errorf("provider spool line %d contains an invalid summary", lineNumber)
			}
			summary = record
		default:
			return nil, fmt.Errorf("provider spool line %d has unsupported record type", lineNumber)
		}
	}
	if err := scanner.Err(); err != nil {
		if limited.N <= 0 {
			return nil, fmt.Errorf("provider spool exceeds %d bytes", maxBytes)
		}
		return nil, fmt.Errorf("read provider spool: %w", err)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("provider spool exceeds %d bytes", maxBytes)
	}
	if summary == nil || summary.SourceDocuments == nil || summary.IndexedChunks == nil {
		return nil, errors.New("provider spool is missing its terminal summary")
	}
	if *summary.SourceDocuments < 0 || *summary.IndexedChunks != int64(len(chunks)) {
		return nil, errors.New("provider spool summary does not match parsed records")
	}
	if len(chunks) == 0 {
		return nil, errors.New("provider spool contains no indexable chunks")
	}
	batches, err := providerIndexBatches(options.Scope, options.OperationID, summary.SourceDigest, chunks)
	if err != nil {
		return nil, err
	}
	return &PreparedProviderIndex{Batches: batches, SourceDocuments: *summary.SourceDocuments}, nil
}

func decodeProviderSpoolRecord(line []byte) (*providerSpoolRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var record providerSpoolRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("provider spool record contains trailing JSON")
	}
	return &record, nil
}

func providerIndexBatches(scope *protocol.KnowledgeScope, operationID, sourceDigest string, chunks []*protocol.KnowledgeIndexChunk) ([]*protocol.KnowledgeIndexBatchRequest, error) {
	var batches []*protocol.KnowledgeIndexBatchRequest
	for offset := 0; offset < len(chunks); {
		end := min(offset+100, len(chunks))
		for {
			batch := &protocol.KnowledgeIndexBatchRequest{
				Scope: proto.CloneOf(scope), OperationId: operationID,
				SourceDigest: sourceDigest, Sequence: int32(len(batches)), Chunks: chunks[offset:end],
			}
			err := pluginsdk.ValidateKnowledgeIndexBatch(batch)
			if err == nil {
				batches = append(batches, batch)
				offset = end
				break
			}
			if status.Code(err) != codes.ResourceExhausted || end-offset == 1 {
				return nil, fmt.Errorf("invalid provider spool batch: %w", err)
			}
			end = offset + (end-offset)/2
		}
	}
	return batches, nil
}

func ProviderSpoolConfirmation(prepared *PreparedProviderIndex, response *protocol.KnowledgeIndexResponse) error {
	if prepared == nil || len(prepared.Batches) == 0 || response == nil {
		return errors.New("provider index confirmation is incomplete")
	}
	first := prepared.Batches[0]
	if !proto.Equal(first.Scope, response.Scope) || first.OperationId != response.OperationId || first.SourceDigest != response.SourceDigest || strings.TrimSpace(response.BackendOperationId) == "" {
		return errors.New("provider index confirmation does not match trusted request")
	}
	return nil
}
