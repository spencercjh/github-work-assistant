package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLitePrivateFileAndReadOnlyOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local ? # space.sqlite")
	if db, err := Open(ctx, "sqlite", path, false); err == nil {
		db.Close()
		t.Fatal("diagnostic open created missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing file was created")
	}
	db, err := Open(ctx, "sqlite", path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE check_data(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO check_data(value) VALUES(?)", "preserved"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, "sqlite", path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err = db.QueryRowContext(ctx, "SELECT value FROM check_data").Scan(&value); err != nil || value != "preserved" {
		t.Fatal("reopen lost data", err)
	}
	link := filepath.Join(filepath.Dir(path), "symlink.sqlite")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(ctx, "sqlite", link, true); err == nil {
		bad.Close()
		t.Fatal("symlink accepted")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(ctx, "sqlite", path, false); err == nil {
		bad.Close()
		t.Fatal("public database accepted")
	}
}
