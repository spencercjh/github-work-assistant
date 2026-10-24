package app

import (
	"bytes"
	"context"
	"github-raft-app/internal/database"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// Database tests never accept a production schema name. CI opts in to requiring SQL rather than silently skipping.
func disposableTestDSN(t *testing.T) string {
	t.Helper()
	path := os.Getenv("GHRAFT_TEST_DSN_FILE")
	if path == "" {
		if os.Getenv("GHRAFT_REQUIRE_DB") == "1" && os.Getenv("GHRAFT_TEST_DRIVER") != "sqlite" {
			t.Fatal("server database integration requires GHRAFT_TEST_DSN_FILE")
		}
		dir := t.TempDir()
		path = filepath.Join(dir, "dsn")
		if err := os.WriteFile(path, []byte(filepath.Join(dir, "assistant_test.sqlite")), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GHRAFT_TEST_DSN_FILE", path)
		t.Setenv("GHRAFT_TEST_DRIVER", "sqlite")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("test DSN must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(b))
	switch testDriver() {
	case "mysql":
		c, err := mysql.ParseDSN(dsn)
		if err != nil || !strings.HasSuffix(c.DBName, "_test") || c.Net != "tcp" || !strings.HasPrefix(c.Addr, "127.0.0.1:") {
			t.Fatal("refusing destructive tests: requires loopback and a database name ending in _test")
		}
	case "postgres":
		c, err := pgx.ParseConfig(dsn)
		if err != nil || c.Host != "127.0.0.1" || !strings.HasSuffix(c.Database, "_test") {
			t.Fatal("refusing destructive PostgreSQL tests")
		}
		for _, f := range c.Fallbacks {
			if f.Host != "127.0.0.1" {
				t.Fatal("non-loopback PostgreSQL fallback")
			}
		}
	case "sqlite":
		rel, err := filepath.Rel(os.TempDir(), dsn)
		if err != nil || !filepath.IsAbs(dsn) || strings.HasPrefix(rel, "..") || !strings.HasSuffix(dsn, "_test.sqlite") {
			t.Fatal("SQLite tests require a disposable _test.sqlite under the temporary directory")
		}
	default:
		t.Fatal("unknown test driver")
	}
	return dsn
}

func freshTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := disposableTestDSN(t)
	s, err := openTestStore(t, context.Background(), dsn, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ghraft_watch_access", "ghraft_work_links", "ghraft_triage", "ghraft_legacy_history", "ghraft_meta", "ghraft_watches", "ghraft_results", "ghraft_events", "ghraft_snapshots", "ghraft_private"} {
		if _, err = s.db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func testDriver() string {
	kind, err := database.Driver(os.Getenv("GHRAFT_TEST_DRIVER"))
	if err != nil {
		return "invalid"
	}
	return kind
}
func openTestStore(t *testing.T, ctx context.Context, dsn string, key []byte) (*Store, error) {
	t.Helper()
	return OpenStoreBackend(ctx, testDriver(), dsn, key, true)
}
