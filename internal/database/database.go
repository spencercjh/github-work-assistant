// Package database contains SQL driver, dialect and transaction differences.
// Application services use the Persistence interface, not this SQL adapter.
package database

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("stored record not found")

type DB struct {
	raw  *sql.DB
	orm  *gorm.DB
	Kind string
}

func Driver(name string) (string, error) {
	switch name {
	case "", "mysql", "seekdb", "tidb":
		return "mysql", nil
	case "postgres", "postgresql":
		return "postgres", nil
	case "sqlite", "sqlite3":
		return "sqlite", nil
	default:
		return "", errors.New("database_driver must be sqlite, mysql, seekdb, tidb or postgres")
	}
}

// create only permits creating a SQLite file and initializing its journal mode.
// Server databases must already exist. Read-only diagnostics pass create=false.
func Open(ctx context.Context, name, dsn string, create bool) (*DB, error) {
	kind, err := Driver(name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("database location is required")
	}
	driver := kind
	if kind == "postgres" {
		driver = "pgx"
	}
	if kind == "sqlite" {
		if !filepath.IsAbs(dsn) {
			return nil, errors.New("SQLite requires an absolute local file path")
		}
		if create {
			if err = os.MkdirAll(filepath.Dir(dsn), 0700); err != nil {
				return nil, errors.New("cannot prepare SQLite directory")
			}
			f, e := os.OpenFile(dsn, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if e == nil {
				f.Close()
			} else if !os.IsExist(e) {
				return nil, errors.New("cannot create SQLite database")
			}
		}
		info, e := os.Lstat(dsn)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("SQLite file must exist, be regular and private (0600); run migrate to create it")
		}
		u := url.URL{Scheme: "file", Path: dsn}
		q := url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(10000)", "foreign_keys(1)", "synchronous(FULL)"}}
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	raw, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, errors.New("invalid database connection configuration")
	}
	raw.SetMaxOpenConns(5)
	raw.SetConnMaxLifetime(3 * time.Minute)
	if err = raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, errors.New("database connection failed; check driver, location, credentials and TLS")
	}
	var dialect gorm.Dialector
	switch kind {
	case "mysql":
		dialect = mysql.New(mysql.Config{Conn: raw})
	case "postgres":
		dialect = postgres.New(postgres.Config{Conn: raw})
	case "sqlite":
		dialect = sqlite.New(sqlite.Config{Conn: raw, DriverName: "sqlite"})
	}
	orm, err := gorm.Open(dialect, &gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true, DisableAutomaticPing: true})
	if err != nil {
		raw.Close()
		return nil, errors.New("database ORM initialization failed")
	}
	db := &DB{raw: raw, orm: orm, Kind: kind}
	if kind == "sqlite" && create {
		var mode string
		if err = raw.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
			raw.Close()
			return nil, errors.New("cannot initialize SQLite WAL journal")
		}
	}
	return db, nil
}

func (d *DB) Close() error                          { return d.raw.Close() }
func (d *DB) PingContext(ctx context.Context) error { return d.raw.PingContext(ctx) }

// Session exposes GORM only to the persistence implementation, never services.
func (d *DB) Session(ctx context.Context) *gorm.DB { return d.orm.WithContext(ctx) }
func (d *DB) Migrate(ctx context.Context, models ...any) error {
	return d.Session(ctx).AutoMigrate(append([]any{&resourceLock{}}, models...)...)
}
func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (int64, error) {
	result := d.Session(ctx).Exec(q, args...)
	return result.RowsAffected, result.Error
}
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.Session(ctx).Raw(q, args...).Rows()
}
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) Row {
	return Row{d.Session(ctx).Raw(q, args...).Row()}
}

type Row struct{ row *sql.Row }

func (r Row) Scan(dest ...any) error { return RecordError(r.row.Scan(dest...)) }
func RecordError(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// GORM owns SQL dialect generation, including conflict handling and placeholders.
func upsert(db *gorm.DB, value any, keys, updates []string) error {
	columns := make([]clause.Column, len(keys))
	for i, k := range keys {
		columns[i] = clause.Column{Name: k}
	}
	conflict := clause.OnConflict{Columns: columns, DoNothing: len(updates) == 0}
	if len(updates) > 0 {
		conflict.DoUpdates = clause.AssignmentColumns(updates)
	}
	return db.Clauses(conflict).Create(value).Error
}
func (d *DB) Upsert(ctx context.Context, value any, keys, updates []string) error {
	return upsert(d.Session(ctx), value, keys, updates)
}

type resourceLock struct {
	Name string `gorm:"size:300;primaryKey"`
	Held int    `gorm:"not null"`
}

func (resourceLock) TableName() string { return "ghraft_locks" }

type Tx struct{ orm *gorm.DB }

func (t *Tx) Session(ctx context.Context) *gorm.DB { return t.orm.WithContext(ctx) }
func (t *Tx) QueryRowContext(ctx context.Context, q string, args ...any) Row {
	return Row{t.Session(ctx).Raw(q, args...).Row()}
}
func (t *Tx) Upsert(ctx context.Context, value any, keys, updates []string) error {
	return upsert(t.Session(ctx), value, keys, updates)
}
func (t *Tx) Commit() error   { return t.orm.Commit().Error }
func (t *Tx) Rollback() error { return t.orm.Rollback().Error }

// BeginWrite serializes one logical resource, including its first observation.
// A missing snapshot cannot be row-locked. A durable lock row closes that race.
// SQLite begins IMMEDIATE; server databases use READ COMMITTED so a read after
// obtaining the lock sees the preceding writer's commit.
func (d *DB) BeginWrite(ctx context.Context, resource string) (*Tx, error) {
	opts := &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	if d.Kind == "sqlite" {
		opts = nil
	}
	orm := d.Session(ctx).Begin(opts)
	if orm.Error != nil {
		return nil, orm.Error
	}
	t := &Tx{orm: orm}
	err := t.Upsert(ctx, &resourceLock{Name: resource}, []string{"name"}, nil)
	if err == nil {
		err = t.Session(ctx).Model(&resourceLock{}).Where("name = ?", resource).UpdateColumn("held", gorm.Expr("1-held")).Error
	}
	if err != nil {
		t.Rollback()
		return nil, err
	}
	return t, nil
}
