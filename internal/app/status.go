package app

import (
	"context"
	"encoding/json"
	"strings"
)

func (s *Store) Status(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	rows, e := s.db.QueryContext(ctx, `SELECT resource_key,last_attempt,last_success,COALESCE(last_error,'') FROM ghraft_watches ORDER BY resource_key`)
	if e != nil {
		return nil, e
	}
	watches := []map[string]any{}
	for rows.Next() {
		var key, err string
		var attempt, success int64
		if e = rows.Scan(&key, &attempt, &success, &err); e != nil {
			rows.Close()
			return nil, e
		}
		watches = append(watches, map[string]any{"resource_key": key, "last_attempt": attempt, "last_success": success, "last_error": err})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out["watches"] = watches
	rows, e = s.db.QueryContext(ctx, `SELECT name,value FROM ghraft_meta WHERE name LIKE 'fork-progress:%' OR name LIKE 'fork-error:%' ORDER BY name`)
	if e != nil {
		return nil, e
	}
	forks := map[string]any{}
	forkErrors := map[string]string{}
	for rows.Next() {
		var name, value string
		if e = rows.Scan(&name, &value); e != nil {
			rows.Close()
			return nil, e
		}
		if strings.HasPrefix(name, "fork-error:") {
			forkErrors[strings.TrimPrefix(name, "fork-error:")] = value
		} else {
			var progress ForkProgress
			if e = json.Unmarshal([]byte(value), &progress); e != nil {
				rows.Close()
				return nil, e
			}
			forks[strings.TrimPrefix(name, "fork-progress:")] = progress
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out["forks"], out["fork_errors"] = forks, forkErrors
	var pendingHooks, retryingHooks int64
	if e = s.db.Session(ctx).Model(&webhookRecord{}).Where("finished_at = 0").Count(&pendingHooks).Error; e != nil {
		return nil, e
	}
	if e = s.db.Session(ctx).Model(&webhookRecord{}).Where("finished_at = 0 AND attempts > 0").Count(&retryingHooks).Error; e != nil {
		return nil, e
	}
	out["webhooks"] = map[string]int64{"pending": pendingHooks, "retrying": retryingHooks}

	for name, q := range map[string]string{"compacted_events": `SELECT COUNT(*) FROM ghraft_events WHERE compacted_at>0`, "pending_delivery": `SELECT COUNT(*) FROM ghraft_events WHERE state='pending'`, "unprocessed": `SELECT COUNT(*) FROM ghraft_events e WHERE NOT EXISTS(SELECT 1 FROM ghraft_triage t WHERE t.event_id=e.event_id) AND NOT EXISTS(SELECT 1 FROM ghraft_results r WHERE r.event_id=e.event_id)`, "legacy_history": `SELECT COUNT(*) FROM ghraft_legacy_history`, "processed": `SELECT COUNT(*) FROM ghraft_triage`} {
		var n int
		if e = s.db.QueryRowContext(ctx, q).Scan(&n); e != nil {
			return nil, e
		}
		out[name] = n
	}
	for _, k := range []string{"notifications-cursor", "notifications-error", "notifications-last-success", "legacy-import"} {
		v, e := s.Meta(ctx, k, "")
		if e != nil {
			return nil, e
		}
		out[k] = v
	}
	return out, nil
}
