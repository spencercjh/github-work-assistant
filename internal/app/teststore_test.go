package app

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// Database tests never accept a production schema name. CI opts in to requiring SQL rather than silently skipping.
func disposableTestDSN(t *testing.T) string {
	t.Helper()
	path := os.Getenv("GHRAFT_TEST_DSN_FILE")
	if path == "" {
		if os.Getenv("GHRAFT_REQUIRE_DB") == "1" {
			t.Fatal("database integration tests are required; GHRAFT_TEST_DSN_FILE is missing")
		}
		t.Skip("isolated database required")
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
	c, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasSuffix(c.DBName, "_test") || c.Net != "tcp" || !strings.HasPrefix(c.Addr, "127.0.0.1:") {
		t.Fatal("refusing destructive tests: requires loopback TCP and a database name ending in _test")
	}
	return dsn
}

func freshTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), disposableTestDSN(t), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ghraft_watch_access", "ghraft_work_links", "ghraft_triage", "ghraft_legacy_history", "ghraft_meta", "ghraft_watches", "ghraft_results", "ghraft_events", "ghraft_snapshots", "ghraft_private"} {
		if _, err = s.DB.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
