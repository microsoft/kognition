// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, pool *pgxpool.Pool, baseline string) error {
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return ApplyMigrations(ctx, pool, source, baseline)
}

func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, source fs.FS, baseline string) error {
	names, err := fs.Glob(source, "*.sql")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("read migrations: found no SQL files")
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	if baseline != "" && !known[baseline] {
		return fmt.Errorf("unknown migration baseline %q", baseline)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(1802465134)`); err != nil {
		return err
	}
	var legacy bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations')`).Scan(&legacy); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp())`); err != nil {
		return err
	}
	rows, err := transaction.Query(ctx, `SELECT name, checksum FROM schema_migrations`)
	if err != nil {
		return err
	}
	applied := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			rows.Close()
			return err
		}
		applied[name] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name := range applied {
		if !known[name] {
			return fmt.Errorf("applied migration %q is missing from this release", name)
		}
	}
	missingEarlier := false
	for _, name := range names {
		if _, exists := applied[name]; !exists {
			missingEarlier = true
		} else if missingEarlier {
			return fmt.Errorf("migration history is not a contiguous prefix: %q follows an unapplied migration", name)
		}
	}
	if len(applied) == 0 && legacy && baseline == "" {
		return fmt.Errorf("existing database has no migration ledger; set KOGNITION_MIGRATION_BASELINE to the last verified applied SQL filename after taking a backup")
	}
	if len(applied) == 0 && !legacy && baseline != "" {
		return fmt.Errorf("cannot baseline an empty database")
	}
	baselining := len(applied) == 0 && legacy
	for _, name := range names {
		content, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		sql := strings.ReplaceAll(string(content), "\r\n", "\n")
		checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(sql)))
		if previous, exists := applied[name]; exists {
			if previous != checksum {
				return fmt.Errorf("migration %q checksum changed", name)
			}
			continue
		}
		if !baselining || name > baseline {
			if _, err := transaction.Exec(ctx, sql); err != nil {
				return fmt.Errorf("apply migration %q: %w", name, err)
			}
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`, name, checksum); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}
