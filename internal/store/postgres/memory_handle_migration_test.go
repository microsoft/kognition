// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryVolumeHandleMigrationPreservesLegacyRecords(t *testing.T) {
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `CREATE TEMP TABLE memory_vectors (
		id UUID NOT NULL, volume_id UUID NOT NULL, content TEXT NOT NULL,
		metadata JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (id, volume_id)
	) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	const recordID = "11111111-1111-4111-8111-111111111111"
	const legacyHandle = "22222222-2222-4222-8222-222222222222"
	const opaqueHandle = "pgvector-" + legacyHandle
	timestamp := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_vectors
		(id, volume_id, content, metadata, created_at)
		VALUES ($1, $2, 'retained legacy memory', '{"proof":"retained"}', $3)`,
		recordID, legacyHandle, timestamp); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("migrations/035_memory_opaque_volume_handles.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := transaction.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("migration must support upgrade and rerun: %v", err)
		}
	}
	var dataType, collation string
	if err := transaction.QueryRow(ctx, `SELECT format_type(attribute.atttypid, attribute.atttypmod),
		coll.collname FROM pg_attribute attribute
		JOIN pg_collation coll ON coll.oid = attribute.attcollation
		WHERE attribute.attrelid = 'memory_vectors'::regclass AND attribute.attname = 'volume_id'`).
		Scan(&dataType, &collation); err != nil || dataType != "text" || collation != "C" {
		t.Fatalf("opaque handle storage = %q/%q, error=%v", dataType, collation, err)
	}
	for _, handle := range []string{opaqueHandle, strings.ToUpper(opaqueHandle), "provider:opaque/volume"} {
		if _, err := transaction.Exec(ctx, `INSERT INTO memory_vectors
			(id, volume_id, content, metadata, created_at) VALUES ($1, $2, $2, '{}', $3)`,
			recordID, handle, timestamp); err != nil {
			t.Fatalf("distinct handle %q must retain its own record ID: %v", handle, err)
		}
	}
	var content, proof, storedHandle string
	var storedAt time.Time
	if err := transaction.QueryRow(ctx, `SELECT volume_id, content, metadata->>'proof', created_at
		FROM memory_vectors WHERE id = $1 AND volume_id = $2`, recordID, legacyHandle).
		Scan(&storedHandle, &content, &proof, &storedAt); err != nil {
		t.Fatal(err)
	}
	if storedHandle != legacyHandle || content != "retained legacy memory" ||
		proof != "retained" || !storedAt.Equal(timestamp) {
		t.Fatal("migration changed existing UUID-backed record identity or payload")
	}
	var count int
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM memory_vectors WHERE id = $1`, recordID).
		Scan(&count); err != nil || count != 4 {
		t.Fatalf("isolated same-ID records=%d, error=%v", count, err)
	}
}

func TestMemoryVolumeHandleMigrationIsTransactional(t *testing.T) {
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `CREATE TEMP TABLE memory_vectors (
		id UUID NOT NULL, volume_id UUID NOT NULL, content TEXT NOT NULL,
		PRIMARY KEY (id, volume_id)
	) ON COMMIT DROP;
	INSERT INTO memory_vectors VALUES (
		'11111111-1111-4111-8111-111111111111',
		'22222222-2222-4222-8222-222222222222', 'retained');
	SAVEPOINT before_upgrade`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("migrations/035_memory_opaque_volume_handles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `ROLLBACK TO SAVEPOINT before_upgrade`); err != nil {
		t.Fatal(err)
	}
	var dataType, content string
	if err := transaction.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute WHERE attrelid = 'memory_vectors'::regclass AND attname = 'volume_id'`).
		Scan(&dataType); err != nil {
		t.Fatal(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT content FROM memory_vectors`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if dataType != "uuid" || content != "retained" {
		t.Fatalf("rollback changed original data/schema: type=%q content=%q", dataType, content)
	}
}

func TestMemoryVolumeHandleMigrationCopiesMatch(t *testing.T) {
	name := "035_memory_opaque_volume_handles.sql"
	embedded, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "helm", "kognition", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(content []byte) []byte {
		return bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	}
	if !bytes.Equal(normalize(embedded), normalize(chart)) {
		t.Fatal("standalone and Helm opaque-handle migrations differ")
	}
}

func TestMemoryVolumeDimensionsMigrationCopiesMatch(t *testing.T) {
	name := "036_memory_volume_dimensions.sql"
	embedded, err := os.ReadFile(filepath.Join("migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "helm", "kognition", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n")),
		bytes.ReplaceAll(chart, []byte("\r\n"), []byte("\n"))) {
		t.Fatal("standalone and Helm dimension migrations differ")
	}
}
