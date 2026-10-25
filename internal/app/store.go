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
	"time"

	"github-raft-app/internal/database"
	"gorm.io/gorm"
)

type Store struct {
	db            *database.DB
	Box           cipher.AEAD
	LookbackHours int
	PrivateSince  time.Time
}

// OpenStoreBackend is the SQL implementation's composition point. New backends
// implement Persistence; driver and dialect selection stay out of services.
func OpenStoreBackend(ctx context.Context, driver, dsn string, key []byte, create bool) (*Store, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	box, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	db, e := database.Open(ctx, driver, dsn, create)
	if e != nil {
		return nil, e
	}
	return &Store{db: db, Box: box, LookbackHours: 6}, nil
}
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.db.Migrate(ctx, &accessRecord{}, &workLinkRecord{}, &watchRecord{}, &metaRecord{}, &legacyRecord{}, &triageRecord{}, &snapshotRecord{}, &eventRecord{}, &secretRecord{}, &resultRecord{}); err != nil {
		return err
	}
	// Old records have no acceptance/import time. Start their retention window
	// at upgrade rather than guessing when delivery or import completed.
	now := time.Now().Unix()
	if err := s.db.Session(ctx).Model(&eventRecord{}).Where("state = ? AND accepted_at = 0", "accepted").UpdateColumn("accepted_at", now).Error; err != nil {
		return err
	}
	return s.db.Session(ctx).Model(&legacyRecord{}).Where("created_at = 0").UpdateColumn("created_at", now).Error
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Store) SaveSecret(ctx context.Context, k string, b []byte, expires int64) error {
	n := make([]byte, s.Box.NonceSize())
	if _, e := rand.Read(n); e != nil {
		return e
	}
	encrypted := s.Box.Seal(n, n, b, []byte(k))
	e := s.db.Upsert(ctx, &secretRecord{k, encrypted, expires}, []string{"secret_key"}, []string{"ciphertext", "expires_at"})
	return e
}
func (s *Store) Secret(ctx context.Context, k string) ([]byte, error) {
	var b []byte
	var exp int64
	e := s.db.QueryRowContext(ctx, `SELECT ciphertext,expires_at FROM ghraft_private WHERE secret_key=?`, k).Scan(&b, &exp)
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
	return s.db.Session(ctx).Where("secret_key = ?", k).Delete(&secretRecord{}).Error
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
	tx, e := s.db.BeginWrite(ctx, "resource:"+key)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var current Snapshot
	_ = json.Unmarshal(body, &current)
	// Keep the restriction after a repository becomes public: historical events
	// can still contain private content. No schema or historical payload rewrite.
	if current.Classification == "private" {
		if e = tx.Upsert(ctx, &metaRecord{"private-watch:" + key, "true"}, []string{"name"}, nil); e != nil {
			return "", e
		}
	}
	classification := current.Classification
	var protected string
	e = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, "private-watch:"+key).Scan(&protected)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return "", e
	}
	if protected == "true" {
		// A change's previous value may still be private after visibility changes.
		classification = "private"
	}
	var previous, previousBody string
	e = tx.QueryRowContext(ctx, `SELECT version_hash,body FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&previous, &previousBody)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return "", e
	}
	if previous == v {
		if e = tx.Session(ctx).Model(&snapshotRecord{}).Where("resource_key = ?", key).UpdateColumn("observed_at", time.Now().Unix()).Error; e != nil {
			return "", e
		}
		return "", tx.Commit()
	}
	nonce := make([]byte, 24)
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	id := hash(append([]byte(key+v), nonce...))
	var since time.Time
	if classification == "private" {
		since = s.PrivateSince
	}
	delta, structured := changes([]byte(previousBody), body, time.Now(), s.LookbackHours, since)
	payload, _ := json.Marshal(map[string]any{"event_id": id, "resource_key": key, "version": v, "previous_version": previous, "first_observation": previous == "", "data_classification": classification, "changes": delta})
	e = tx.Upsert(ctx, &snapshotRecord{key, v, string(body), time.Now().Unix()}, []string{"resource_key"}, []string{"version_hash", "body", "observed_at"})
	if e != nil {
		return "", e
	}
	if structured && len(delta) == 0 {
		return "", tx.Commit()
	}
	e = tx.Session(ctx).Create(&eventRecord{EventID: id, ResourceKey: key, VersionHash: v, Payload: string(payload), State: "pending", CreatedAt: time.Now().Unix()}).Error
	if e != nil {
		return "", e
	}
	return id, tx.Commit()
}
func (s *Store) Snapshot(ctx context.Context, key string) (json.RawMessage, string, error) {
	var b string
	var v string
	e := s.db.QueryRowContext(ctx, `SELECT body,version_hash FROM ghraft_snapshots WHERE resource_key=?`, key).Scan(&b, &v)
	return json.RawMessage(b), v, e
}
func (s *Store) Events(ctx context.Context, pending bool) ([]Event, error) {
	return s.EventsPage(ctx, pending, 0)
}
func (s *Store) EventsPage(ctx context.Context, pending bool, offset int) ([]Event, error) {
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
	rows, e := s.db.QueryContext(ctx, q, args...)
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
	tx, _, err := s.eventWrite(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if success {
		if !json.Valid(receipt) {
			return errors.New("invalid delivery receipt")
		}
		err = tx.Session(ctx).Model(&eventRecord{}).Where("event_id = ? AND state = ?", id, "pending").Updates(map[string]any{"state": "accepted", "receipt": string(receipt), "accepted_at": time.Now().Unix(), "attempts": gorm.Expr("attempts+1")}).Error
		if err == nil {
			err = tx.Session(ctx).Where("name = ?", "external-delivery:"+id).Delete(&metaRecord{}).Error
		}
	} else {
		delay := min(3600, 30*(1<<min(attempts, 7)))
		err = tx.Session(ctx).Model(&eventRecord{}).Where("event_id = ? AND state = ?", id, "pending").Updates(map[string]any{"attempts": gorm.Expr("attempts+1"), "next_attempt": time.Now().Unix() + int64(delay)}).Error
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Result records link evidence to Raft; they never mutate Raft task status.
func (s *Store) Result(ctx context.Context, id, report string, body []byte) error {
	tx, _, err := s.eventWrite(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveReport(ctx, tx, id, report, body); err != nil {
		return err
	}
	return tx.Commit()
}
