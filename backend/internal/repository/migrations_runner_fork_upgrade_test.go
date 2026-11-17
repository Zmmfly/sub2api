package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
)

// TestForkMigrationUpgradePaths is an opt-in real PostgreSQL check. The DSN
// must point to a disposable server with CREATE DATABASE permission. Historical
// directories contain immutable SQL files extracted from the named revisions.
func TestForkMigrationUpgradePaths(t *testing.T) {
	dsn := os.Getenv("SUB2API_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("set SUB2API_MIGRATION_TEST_DSN for real PostgreSQL migration checks")
	}
	oldRelease := os.Getenv("SUB2API_MIGRATION_OLD_RELEASE_DIR")
	oldFork := os.Getenv("SUB2API_MIGRATION_OLD_FORK_DIR")
	if oldRelease == "" || oldFork == "" {
		t.Fatal("both historical migration directories are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	for _, tc := range []struct{ name, prior string }{
		{"fresh", ""}, {"from_dd5db78", oldRelease}, {"from_existing_fork_251", oldFork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("sub2api_migration_%d", time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name); err != nil {
					t.Errorf("drop test database: %v", err)
				}
			}()
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			u.Path = "/" + name
			db, err := sql.Open("postgres", u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			prior := map[string]string{}
			if tc.prior != "" {
				if err := applyMigrationsFS(ctx, db, os.DirFS(tc.prior)); err != nil {
					t.Fatalf("historical install: %v", err)
				}
				rows, err := db.QueryContext(ctx, "SELECT filename, checksum FROM schema_migrations")
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var name, checksum string
					if err := rows.Scan(&name, &checksum); err != nil {
						t.Fatal(err)
					}
					prior[name] = checksum
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				_ = rows.Close()
				if _, err := db.ExecContext(ctx, "INSERT INTO groups(name, rate_multiplier) VALUES ('migration_sentinel', 1.2345)"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApplyMigrations(ctx, db); err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			files, err := fs.Glob(migrations.FS, "*.sql")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range files {
				b, err := fs.ReadFile(migrations.FS, name)
				if err != nil {
					t.Fatal(err)
				}
				content := strings.TrimSpace(string(b))
				if content == "" {
					continue
				}
				var checksum string
				if err := db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename=$1", name).Scan(&checksum); err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
				if previous, ok := prior[name]; ok {
					want = previous
				}
				if checksum != want {
					t.Fatalf("checksum changed for %s: %s != %s", name, checksum, want)
				}
			}
			var both int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename IN ('251_group_force_openai_ultrafast.sql','251_group_usage_rollup_nonblocking_build.sql')").Scan(&both); err != nil || both != 2 {
				t.Fatalf("251 records: count=%d err=%v", both, err)
			}
			var forceAt, rollupAt time.Time
			if err := db.QueryRowContext(ctx, "SELECT applied_at FROM schema_migrations WHERE filename='251_group_force_openai_ultrafast.sql'").Scan(&forceAt); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, "SELECT applied_at FROM schema_migrations WHERE filename='251_group_usage_rollup_nonblocking_build.sql'").Scan(&rollupAt); err != nil {
				t.Fatal(err)
			}
			if tc.name == "from_existing_fork_251" {
				if !forceAt.After(rollupAt) {
					t.Fatal("upgrade did not preserve the already-applied later-sorting 251 migration")
				}
			} else if !forceAt.Before(rollupAt) {
				t.Fatal("new migrations did not execute in complete-filename order")
			}
			var columns int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='groups' AND column_name='force_openai_ultrafast') OR (table_name='usage_group_rollup_state' AND column_name IN ('expired_before','building_until','build_dirty_from')))").Scan(&columns); err != nil || columns != 4 {
				t.Fatalf("251 columns: count=%d err=%v", columns, err)
			}
			if tc.prior != "" {
				var multiplier string
				var ultrafast bool
				if err := db.QueryRowContext(ctx, "SELECT rate_multiplier::text,force_openai_ultrafast FROM groups WHERE name='migration_sentinel'").Scan(&multiplier, &ultrafast); err != nil || multiplier != "1.2345" || ultrafast {
					t.Fatalf("prior group changed: %q ultrafast=%v err=%v", multiplier, ultrafast, err)
				}
			}
			var before, after string
			query := "SELECT md5(string_agg(filename||checksum||applied_at::text, ',' ORDER BY filename)) FROM schema_migrations"
			if err := db.QueryRowContext(ctx, query).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := ApplyMigrations(ctx, db); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			if err := db.QueryRowContext(ctx, query).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("repeat changed migration records")
			}
			t.Logf("verified %d migration files, both 251 identities, existing checksums and repeat stability", len(files))
		})
	}
}
