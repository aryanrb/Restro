package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/oneaccord/restro/api/internal/db"
)

// TestRunMigrationsIdempotent verifies migrations apply, are safe to re-run, and
// that the tern up/down separator is parsed correctly (only the "up" runs, so
// schema_baseline exists afterward). Skips unless TEST_DATABASE_URL is set; CI and
// the dev compose provide it.
func TestRunMigrationsIdempotent(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB integration tests")
	}

	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer pool.Close()

	// Idempotent: applying migrations twice is a no-op (safe on every boot). This
	// also proves the tern up/down separator parses — only the "up" runs, so
	// schema_baseline exists rather than being created-then-dropped.
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations (should be a no-op): %v", err)
	}

	// The table exists only if the up migration ran and the down did not.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_baseline").Scan(&n); err != nil {
		t.Fatalf("schema_baseline missing after migrate: %v", err)
	}
}
