package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"
)

type Change struct {
	Object string `json:"object"`
	Before any    `json:"before"`
	After  any    `json:"after"`
	Kind   string `json:"kind"`
}

func changes(previous, current []byte, now time.Time, lookback int) ([]Change, bool) {
	var old, new Snapshot
	if json.Unmarshal(current, &new) != nil || new.Objects == nil {
		return nil, false
	}
	_ = json.Unmarshal(previous, &old)
	out := []Change{}
	keys := []string{}
	for k := range new.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := new.Objects[k]
		before, existed := old.Objects[k]
		if existed && reflect.DeepEqual(before, v) {
			continue
		}
		kind := "changed"
		if !existed {
			kind = "added"
		}
		if old.Objects == nil {
			kind = "backfill"
			if k == "issue" || k == "pull" {
				kind = "baseline"
			} else {
				prefix := strings.SplitN(k, ":", 2)[0]
				if prefix != "comment" && prefix != "review" && prefix != "review_comment" {
					continue
				}
				recent := false
				for _, f := range []string{"created_at", "updated_at", "submitted_at"} {
					t, e := time.Parse(time.RFC3339, str(v[f]))
					if e == nil && !t.Before(now.Add(-time.Duration(lookback)*time.Hour)) {
						recent = true
					}
				}
				if !recent {
					continue
				}
			}
		}
		var b any
		if existed {
			b = before
		}
		out = append(out, Change{k, b, v, kind})
	}
	// Disappearance is observable, but does not prove deletion: API retention or visibility can change.
	// Emit a qualified fact so every changed snapshot has a current event that an agent can act on.
	missing := []string{}
	for k := range old.Objects {
		if _, ok := new.Objects[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		out = append(out, Change{k, old.Objects[k], nil, "not_observed"})
	}
	return out, true
}
func (s *Store) RegisterWatch(ctx context.Context, w Watch) error {
	_, e := s.DB.ExecContext(ctx, `INSERT IGNORE INTO ghraft_watches(resource_key,repo,number) VALUES(?,?,?)`, w.Key(), w.Repo, w.Number)
	return e
}
func (s *Store) Watches(ctx context.Context) ([]Watch, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT repo,number FROM ghraft_watches ORDER BY resource_key`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		if e = rows.Scan(&w.Repo, &w.Number); e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (s *Store) Meta(ctx context.Context, key, defaultValue string) (string, error) {
	var v string
	e := s.DB.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, key).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		return defaultValue, nil
	}
	return v, e
}
func (s *Store) PutMeta(ctx context.Context, key, value string) error {
	_, e := s.DB.ExecContext(ctx, `INSERT INTO ghraft_meta(name,value) VALUES(?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, key, value)
	return e
}
func (s *Store) CollectionStatus(ctx context.Context, key string, err error) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_, e := s.DB.ExecContext(ctx, `UPDATE ghraft_watches SET last_attempt=?, last_success=CASE WHEN ?='' THEN ? ELSE last_success END,last_error=? WHERE resource_key=?`, time.Now().Unix(), msg, time.Now().Unix(), msg, key)
	return e
}

// Only the Agent judges significance; transport acceptance cannot mark processing complete.
type Triage struct {
	EventID   string `json:"event_id"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason"`
	MessageID string `json:"message_id"`
	Target    string `json:"target"`
}

func (s *Store) RecordTriage(ctx context.Context, t Triage) error {
	if !idRE.MatchString(t.EventID) || (t.Outcome != "quiet" && t.Outcome != "notified") || len(t.Reason) == 0 || len(t.Reason) > 8000 {
		return errors.New("invalid triage")
	}
	if t.Outcome == "notified" && (!uuidRE.MatchString(t.MessageID) || !strings.HasPrefix(t.Target, "dm:@")) {
		return errors.New("notified requires real DM message UUID and destination")
	}
	var n int
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ghraft_events WHERE event_id=?`, t.EventID).Scan(&n); e != nil || n != 1 {
		return errors.New("unknown event")
	}
	b := string(canonical(t))
	_, e := s.DB.ExecContext(ctx, `INSERT IGNORE INTO ghraft_triage(event_id,body,created_at) VALUES(?,?,?)`, t.EventID, b, time.Now().Unix())
	if e != nil {
		return e
	}
	var existing string
	e = s.DB.QueryRowContext(ctx, `SELECT body FROM ghraft_triage WHERE event_id=?`, t.EventID).Scan(&existing)
	if e != nil {
		return e
	}
	if existing != b {
		return errors.New("triage already recorded with different content")
	}
	return nil
}
func (s *Store) Unprocessed(ctx context.Context) ([]map[string]any, error) {
	return s.unprocessedPage(ctx, 0)
}
func (s *Store) unprocessedPage(ctx context.Context, offset int) ([]map[string]any, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT e.event_id,e.resource_key,e.version_hash,e.state,e.payload FROM ghraft_events e LEFT JOIN ghraft_triage t ON t.event_id=e.event_id WHERE t.event_id IS NULL AND NOT EXISTS(SELECT 1 FROM ghraft_results r WHERE r.event_id=e.event_id) ORDER BY e.created_at,e.event_id LIMIT 100 OFFSET ?`, offset)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, key, version, state, p string
		if e = rows.Scan(&id, &key, &version, &state, &p); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"event_id": id, "resource_key": key, "version": version, "state": state, "payload": json.RawMessage(p)})
	}
	return out, rows.Err()
}

type LegacyImport struct {
	Source  string `json:"source"`
	Login   string `json:"login"`
	Cursor  string `json:"cursor"`
	Watches []struct {
		Repo     string  `json:"repo"`
		Number   int     `json:"number"`
		Private  bool    `json:"private"`
		Objects  Objects `json:"objects"`
		Observed int64   `json:"observed_at"`
	} `json:"watches"`
	History []map[string]any `json:"history"`
	Pending int              `json:"pending"`
}

func (s *Store) ImportLegacy(ctx context.Context, c Config, b []byte) error {
	var in LegacyImport
	if json.Unmarshal(b, &in) != nil || in.Source != "github-assistant-pilot-v1" || !strings.EqualFold(in.Login, c.GitHubLogin) || len(in.Watches) == 0 || in.Pending != 0 {
		return errors.New("invalid import identity or unprocessed legacy batches")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var old string
	e = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name='legacy-import' FOR UPDATE`).Scan(&old)
	fingerprint := hash(b)
	if e == nil {
		if old == fingerprint {
			return nil
		}
		return errors.New("legacy import already completed")
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	for _, w := range in.Watches {
		watch := Watch{w.Repo, w.Number}
		if !repoRE.MatchString(w.Repo) || strings.Contains(w.Repo, "..") || w.Number < 1 || w.Objects == nil || w.Private {
			return errors.New("import watch outside authorization")
		}
		classification := "public"
		if w.Private {
			classification = "private"
		}
		body := canonical(Snapshot{watch.Key(), classification, w.Objects})
		if _, e = tx.ExecContext(ctx, `INSERT IGNORE INTO ghraft_watches(resource_key,repo,number) VALUES(?,?,?)`, watch.Key(), watch.Repo, watch.Number); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO ghraft_snapshots(resource_key,version_hash,body,observed_at) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE version_hash=VALUES(version_hash),body=VALUES(body),observed_at=VALUES(observed_at)`, watch.Key(), hash(body), string(body), w.Observed); e != nil {
			return e
		}
	}
	for _, row := range in.History {
		marker := str(row["marker"])
		if marker == "" {
			return errors.New("missing legacy marker")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO ghraft_legacy_history(marker,body) VALUES(?,?)`, marker, string(canonical(row))); e != nil {
			return e
		}
	}
	for k, v := range map[string]string{"legacy-import": fingerprint, "notifications-cursor": in.Cursor} {
		if _, e = tx.ExecContext(ctx, `INSERT INTO ghraft_meta(name,value) VALUES(?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, k, v); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Historical decisions are evidence for the Agent, not a global suppression rule.
// A new opinion on the same PR still gets its own event and judgment.
func (s *Store) History(ctx context.Context, key string) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	rows, e := s.DB.QueryContext(ctx, `SELECT body FROM ghraft_legacy_history WHERE body LIKE ?`, `%"watch":"`+strings.ToLower(key)+`"%`)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, json.RawMessage(b))
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	rows, e = s.DB.QueryContext(ctx, `SELECT t.body FROM ghraft_triage t JOIN ghraft_events e ON e.event_id=t.event_id WHERE e.resource_key=? ORDER BY t.created_at DESC LIMIT 20`, key)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, json.RawMessage(b))
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	rows, e = s.DB.QueryContext(ctx, `SELECT r.body FROM ghraft_results r JOIN ghraft_events e ON e.event_id=r.event_id WHERE e.resource_key=? ORDER BY r.created_at DESC LIMIT 20`, key)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
