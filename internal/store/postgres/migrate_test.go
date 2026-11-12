// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type failingMigrationGlob struct {
	fs.FS
	err error
}

func (source failingMigrationGlob) Glob(string) ([]string, error) {
	return nil, source.err
}

func TestMigrationRunnerRejectsUnavailableSource(t *testing.T) {
	t.Run("discovery error", func(t *testing.T) {
		cause := errors.New("migration discovery unavailable")
		err := ApplyMigrations(context.Background(), nil, failingMigrationGlob{FS: fstest.MapFS{}, err: cause}, "")
		if !errors.Is(err, cause) {
			t.Fatalf("migration error = %v, want discovery cause", err)
		}
	})
	t.Run("empty source", func(t *testing.T) {
		err := ApplyMigrations(context.Background(), nil, fstest.MapFS{}, "")
		if err == nil || err.Error() != "read migrations: found no SQL files" {
			t.Fatalf("empty source error = %v", err)
		}
	})
}

func TestMigrationRunnerPreservesDataAndRejectsDrift(t *testing.T) {
	ctx := context.Background()
	config := testPool.Config()
	schema := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := testPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = testPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	source := fstest.MapFS{
		"001_base.sql":    {Data: []byte("CREATE TABLE records (value TEXT); INSERT INTO records VALUES ('retained');")},
		"002_upgrade.sql": {Data: []byte("ALTER TABLE records ADD COLUMN upgraded BOOLEAN NOT NULL DEFAULT true;")},
	}
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			if err := ApplyMigrations(ctx, pool, source, ""); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM records WHERE value = 'retained' AND upgraded").Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained rows=%d: %v", count, err)
	}
	source["003_broken.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE partial_change (value TEXT); SELECT missing_column FROM records;")}
	if err := ApplyMigrations(ctx, pool, source, ""); err == nil {
		t.Fatal("broken migration accepted")
	}
	var partial *string
	if err := pool.QueryRow(ctx, "SELECT to_regclass('partial_change')::text").Scan(&partial); err != nil || partial != nil {
		t.Fatalf("partial migration committed: %v", err)
	}
	delete(source, "003_broken.sql")
	source["001_late.sql"] = &fstest.MapFile{Data: []byte(`SELECT 1`)}
	if err := ApplyMigrations(ctx, pool, source, ""); err == nil || !strings.Contains(err.Error(), "contiguous prefix") {
		t.Fatalf("late historical migration should be rejected: %v", err)
	}
	delete(source, "001_late.sql")
	source["002_upgrade.sql"].Data = []byte("SELECT 1;")
	if err := ApplyMigrations(ctx, pool, source, ""); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("drift accepted: %v", err)
	}
}

func TestMigrationRunnerRequiresExplicitLegacyBaseline(t *testing.T) {
	ctx := context.Background()
	config := testPool.Config()
	schema := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := testPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = testPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	source := fstest.MapFS{
		"001_base.sql":    {Data: []byte("CREATE TABLE records (value TEXT); INSERT INTO records VALUES ('retained');")},
		"002_upgrade.sql": {Data: []byte("ALTER TABLE records ADD COLUMN upgraded BOOLEAN NOT NULL DEFAULT true;")},
	}
	if err := ApplyMigrations(ctx, pool, source, "001_base.sql"); err == nil {
		t.Fatal("empty database baseline accepted")
	}
	if _, err := pool.Exec(ctx, string(source["001_base.sql"].Data)); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, source, ""); err == nil {
		t.Fatal("implicit legacy baseline accepted")
	}
	if err := ApplyMigrations(ctx, pool, source, "unknown.sql"); err == nil {
		t.Fatal("unknown baseline accepted")
	}
	for range 2 {
		if err := ApplyMigrations(ctx, pool, source, "001_base.sql"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM records WHERE value = 'retained' AND upgraded").Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy data lost: count=%d error=%v", count, err)
	}
}
