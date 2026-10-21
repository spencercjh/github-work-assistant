package app

import "context"

func (s *Store) Status(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	rows, e := s.DB.QueryContext(ctx, `SELECT resource_key,last_attempt,last_success,COALESCE(last_error,'') FROM ghraft_watches ORDER BY resource_key`)
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
	for name, q := range map[string]string{"pending_delivery": `SELECT COUNT(*) FROM ghraft_events WHERE state='pending'`, "unprocessed": `SELECT COUNT(*) FROM ghraft_events e WHERE NOT EXISTS(SELECT 1 FROM ghraft_triage t WHERE t.event_id=e.event_id) AND NOT EXISTS(SELECT 1 FROM ghraft_results r WHERE r.event_id=e.event_id)`, "legacy_history": `SELECT COUNT(*) FROM ghraft_legacy_history`, "processed": `SELECT COUNT(*) FROM ghraft_triage`} {
		var n int
		if e = s.DB.QueryRowContext(ctx, q).Scan(&n); e != nil {
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
