package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Store struct {
	DB            *sql.DB
	Box           cipher.AEAD
	LookbackHours int
}

func OpenStore(ctx context.Context, dsn string, key []byte) (*Store, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	box, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		return nil, errors.New("invalid database configuration")
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(3 * time.Minute)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, errors.New("database connection failed")
	}
	return &Store{DB: db, Box: box, LookbackHours: 6}, nil
}
func (s *Store) Migrate(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS ghraft_watch_access (resource_key VARCHAR(240) PRIMARY KEY, source VARCHAR(16) NOT NULL, blocked BOOL NOT NULL DEFAULT FALSE)`,
		`CREATE TABLE IF NOT EXISTS ghraft_work_links (resource_key VARCHAR(240) PRIMARY KEY, task_id CHAR(36) NOT NULL, target VARCHAR(240) NOT NULL, event_id CHAR(64) NOT NULL, evidence_version CHAR(64) NOT NULL, guide_version CHAR(64) NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_watches (resource_key VARCHAR(240) PRIMARY KEY, repo VARCHAR(220) NOT NULL, number INT NOT NULL, last_attempt BIGINT NOT NULL DEFAULT 0, last_success BIGINT NOT NULL DEFAULT 0, last_error TEXT)`,
		`CREATE TABLE IF NOT EXISTS ghraft_meta (name VARCHAR(120) PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_legacy_history (marker VARCHAR(160) PRIMARY KEY, body MEDIUMTEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_triage (event_id CHAR(64) PRIMARY KEY, body TEXT NOT NULL, created_at BIGINT NOT NULL)`,

		`CREATE TABLE IF NOT EXISTS ghraft_snapshots (resource_key VARCHAR(240) PRIMARY KEY, version_hash CHAR(64) NOT NULL, body MEDIUMTEXT NOT NULL, observed_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_events (event_id CHAR(64) PRIMARY KEY, resource_key VARCHAR(240) NOT NULL, version_hash CHAR(64) NOT NULL, payload MEDIUMTEXT NOT NULL, state VARCHAR(16) NOT NULL, receipt TEXT, attempts INT NOT NULL DEFAULT 0, next_attempt BIGINT NOT NULL DEFAULT 0, created_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_private (secret_key VARCHAR(200) PRIMARY KEY, ciphertext BLOB NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ghraft_results (event_id CHAR(64) NOT NULL, report_id VARCHAR(160) NOT NULL, body MEDIUMTEXT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY(event_id,report_id))`,
	} {
		if _, e := s.DB.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	for _, q := range []string{`ALTER TABLE ghraft_events MODIFY payload MEDIUMTEXT NOT NULL`, `ALTER TABLE ghraft_results MODIFY body MEDIUMTEXT NOT NULL`} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Store) SaveSecret(ctx context.Context, k string, b []byte, expires int64) error {
	n := make([]byte, s.Box.NonceSize())
	if _, e := rand.Read(n); e != nil {
		return e
	}
	encrypted := s.Box.Seal(n, n, b, []byte(k))
	_, e := s.DB.ExecContext(ctx, `INSERT INTO ghraft_private(secret_key,ciphertext,expires_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE ciphertext=VALUES(ciphertext),expires_at=VALUES(expires_at)`, k, encrypted, expires)
	return e
}
func (s *Store) Secret(ctx context.Context, k string) ([]byte, error) {
	var b []byte
	var exp int64
	e := s.DB.QueryRowContext(ctx, `SELECT ciphertext,expires_at FROM ghraft_private WHERE secret_key=?`, k).Scan(&b, &exp)
	if e != nil {
		return nil, e
	}
	if exp <= time.Now().Unix() || len(b) < s.Box.NonceSize() {
		return nil, errors.New("expired or malformed secret")
	}
	n := s.Box.NonceSize()
	return s.Box.Open(nil, b[:n], b[n:], []byte(k))
}
func (s *Store) DeleteSecret(ctx context.Context, k string) error {
	_, e := s.DB.ExecContext(ctx, `DELETE FROM ghraft_private WHERE secret_key=?`, k)
	return e
}

type Event struct {
	ID       string          `json:"event_id"`
	Key      string          `json:"resource_key"`
	Version  string          `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	State    string          `json:"state"`
	Receipt  json.RawMessage `json:"receipt,omitempty"`
	Attempts int             `json:"attempts"`
}

// A version transition gets its own event, including A->B->A. Snapshot+outbox commit together.
func (s *Store) Observe(ctx context.Context, key string, body []byte) (string, error) {
	v := hash(body)
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var previous, previousBody string
	e = tx.QueryRowContext(ctx, `SELECT version_hash,body FROM ghraft_snapshots WHERE resource_key=? FOR UPDATE`, key).Scan(&previous, &previousBody)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	if previous == v {
		if _, e = tx.ExecContext(ctx, `UPDATE ghraft_snapshots SET observed_at=? WHERE resource_key=?`, time.Now().Unix(), key); e != nil {
			return "", e
		}
		return "", tx.Commit()
	}
	nonce := make([]byte, 24)
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	id := hash(append([]byte(key+v), nonce...))
	delta, structured := changes([]byte(previousBody), body, time.Now(), s.LookbackHours)
	var current Snapshot
	_ = json.Unmarshal(body, &current)
	payload, _ := json.Marshal(map[string]any{"event_id": id, "resource_key": key, "version": v, "previous_version": previous, "first_observation": previous == "", "data_classification": current.Classification, "changes": delta})
	_, e = tx.ExecContext(ctx, `INSERT INTO ghraft_snapshots(resource_key,version_hash,body,observed_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE version_hash=VALUES(version_hash),body=VALUES(body),observed_at=VALUES(observed_at)`, key, v, string(body), time.Now().Unix())
	if e != nil {
		return "", e
	}
	if structured && len(delta) == 0 {
		return "", tx.Commit()
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO ghraft_events(event_id,resource_key,version_hash,payload,state,created_at) VALUES(?,?,?,?, 'pending',?)`, id, key, v, string(payload), time.Now().Unix())
	if e != nil {
		return "", e
	}
	return id, tx.Commit()
}
func (s *Store) Snapshot(ctx context.Context, key string) (json.RawMessage, string, error) {
	var b string
	var v string
	e := s.DB.QueryRowContext(ctx, `SELECT body,version_hash FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&b, &v)
	return json.RawMessage(b), v, e
}
func (s *Store) Events(ctx context.Context, pending bool) ([]Event, error) {
	return s.eventsPage(ctx, pending, 0)
}
func (s *Store) eventsPage(ctx context.Context, pending bool, offset int) ([]Event, error) {
	q := `SELECT event_id,resource_key,version_hash,payload,state,receipt,attempts FROM ghraft_events`
	if pending {
		q += ` WHERE state='pending' AND next_attempt<=?`
	}
	if pending {
		q += ` ORDER BY created_at,event_id LIMIT 100 OFFSET ?`
	} else {
		q += ` ORDER BY created_at DESC,event_id DESC LIMIT 100 OFFSET ?`
	}
	args := []any{}
	if pending {
		args = append(args, time.Now().Unix())
	}
	args = append(args, offset)
	rows, e := s.DB.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var x Event
		var p string
		var receipt sql.NullString
		if e = rows.Scan(&x.ID, &x.Key, &x.Version, &p, &x.State, &receipt, &x.Attempts); e != nil {
			return nil, e
		}
		x.Payload = json.RawMessage(p)
		if receipt.Valid {
			x.Receipt = json.RawMessage(receipt.String)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) Delivery(ctx context.Context, id string, receipt []byte, success bool, attempts int) error {
	if success {
		_, e := s.DB.ExecContext(ctx, `UPDATE ghraft_events SET state='accepted',receipt=?,attempts=attempts+1 WHERE event_id=?`, string(receipt), id)
		return e
	}
	delay := min(3600, 30*(1<<min(attempts, 7)))
	_, e := s.DB.ExecContext(ctx, `UPDATE ghraft_events SET attempts=attempts+1,next_attempt=? WHERE event_id=?`, time.Now().Unix()+int64(delay), id)
	return e
}

// Result records link evidence to Raft; they never mutate Raft task status.
func (s *Store) Result(ctx context.Context, id, report string, body []byte) error {
	var n int
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ghraft_events WHERE event_id=?`, id).Scan(&n); e != nil {
		return e
	}
	if n != 1 {
		return errors.New("unknown event")
	}
	_, e := s.DB.ExecContext(ctx, `INSERT IGNORE INTO ghraft_results(event_id,report_id,body,created_at) VALUES(?,?,?,?)`, id, report, string(body), time.Now().Unix())
	if e != nil {
		return e
	}
	var saved string
	if e = s.DB.QueryRowContext(ctx, `SELECT body FROM ghraft_results WHERE event_id=? AND report_id=?`, id, report).Scan(&saved); e != nil {
		return e
	}
	if saved != string(body) {
		return fmt.Errorf("report id already used with different content")
	}
	return nil
}
