package app

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPublicDiscoveryBoundary(t *testing.T) {
	row := map[string]any{"subject": map[string]any{"type": "PullRequest", "url": "https://api.github.com/repos/Project-HAMi/HAMi/pulls/3259"}, "repository": map[string]any{"private": false}}
	if w, ok := (GH{}).notificationWatch(row); !ok || w.Key() != "project-hami/hami#3259" {
		t.Fatal("public notification lost")
	}
	row["repository"] = map[string]any{"private": true}
	if _, ok := (GH{}).notificationWatch(row); ok {
		t.Fatal("private discovery allowed")
	}
	row["repository"] = map[string]any{}
	if _, ok := (GH{}).notificationWatch(row); ok {
		t.Fatal("unknown visibility allowed")
	}
	row["repository"] = map[string]any{"private": false}
	obj(row["subject"])["url"] = "https://evil.example/repos/Project-HAMi/HAMi/pulls/3259"
	if _, ok := (GH{}).notificationWatch(row); ok {
		t.Fatal("untrusted API URL")
	}
}
func TestLegacyObjectIdentityAndNoise(t *testing.T) {
	var row map[string]any
	_ = json.Unmarshal([]byte(`{"id":4314120299,"body":"new","user":{"login":"reviewer","followers":10}}`), &row)
	key, n, _ := normalizeRow("review_comment", row)
	if key != "review_comment:4314120299" {
		t.Fatal("legacy key changed", key)
	}
	obj(row["user"])["followers"] = 20
	_, m, _ := normalizeRow("review_comment", row)
	if !bytes.Equal(canonical(n), canonical(m)) {
		t.Fatal("account statistics cause event")
	}
	ref := map[string]any{"event": "cross-referenced", "source": map[string]any{"type": "issue", "issue": map[string]any{"id": 1, "number": 2, "repository": map[string]any{"private": true}}}}
	if _, _, ok := normalizeRow("timeline", ref); ok {
		t.Fatal("private reference leaked")
	}
	obj(obj(ref["source"])["issue"])["repository"] = map[string]any{"private": false, "forks": 1}
	a, _, ok := normalizeRow("timeline", ref)
	if !ok {
		t.Fatal("public reference lost")
	}
	obj(obj(obj(ref["source"])["issue"])["repository"])["forks"] = 2
	b, _, _ := normalizeRow("timeline", ref)
	if a != b {
		t.Fatal("crossref stats create fake event")
	}
}
func TestDeltaBackfillAndEdits(t *testing.T) {
	now := time.Now().UTC()
	s := Snapshot{Key: "o/r#1", Classification: "public", Objects: Objects{"issue": {"title": "a"}, "review_comment:1": {"body": "old", "created_at": now.Add(-time.Hour).Format(time.RFC3339)}, "comment:2": {"body": "older", "created_at": now.Add(-24 * time.Hour).Format(time.RFC3339)}}}
	b := canonical(s)
	d, ok := changes(nil, b, now, 6, time.Time{})
	if !ok || len(d) != 2 {
		t.Fatal("first observation did not catch recent comment", d)
	}
	if d, _ := changes(b, b, now, 6, time.Time{}); len(d) != 0 {
		t.Fatal("same baseline replayed")
	}
	s.Objects["review_comment:1"]["body"] = "edited"
	d, _ = changes(b, canonical(s), now, 6, time.Time{})
	if len(d) != 1 || d[0].Kind != "changed" || obj(d[0].Before)["body"] != "old" {
		t.Fatal("edit evidence lost", d)
	}
	delete(s.Objects, "review_comment:1")
	delete(s.Objects, "comment:2")
	d, _ = changes(b, canonical(s), now, 6, time.Time{})
	if len(d) != 2 || d[0].Kind != "not_observed" || d[1].Kind != "not_observed" {
		t.Fatal("missing objects must produce qualified observations, not asserted deletion", d)
	}
}
func TestLegacyImportAndTriagePersistence(t *testing.T) {
	ctx := context.Background()
	s := freshTestStore(t)
	raw := []byte(`{"source":"github-assistant-pilot-v1","login":"tester","cursor":"2026-10-19T00:00:00Z","pending":0,"watches":[{"repo":"o/r","number":1,"private":false,"observed_at":1,"objects":{"issue":{"title":"old"},"review_comment:4314120299":{"body":"seen"}}}],"history":[{"marker":"old-marker","watch":"o/r#1","outcome":"notified","message":"real-legacy-ref"}]}`)
	cfg := Config{GitHubLogin: "tester"}
	if e := s.ImportLegacy(ctx, cfg, raw); e != nil {
		t.Fatal(e)
	}
	if e := s.ImportLegacy(ctx, cfg, raw); e != nil {
		t.Fatal("idempotent import", e)
	}
	hist, err := s.History(ctx, "o/r#1")
	if err != nil || len(hist) != 1 {
		t.Fatal("legacy notification history missing", err, hist)
	}
	body, _, e := s.Snapshot(ctx, "o/r#1")
	if e != nil {
		t.Fatal(e)
	}
	if id, e := s.Observe(ctx, "o/r#1", body); e != nil || id != "" {
		t.Fatal("import replayed historical notification", id, e)
	}
	var current Snapshot
	_ = json.Unmarshal(body, &current)
	current.Objects["review_comment:4314120299"]["body"] = "new opinion"
	id, e := s.Observe(ctx, "o/r#1", canonical(current))
	if e != nil || id == "" {
		t.Fatal("post-import edit lost", e)
	}
	pending, e := s.UnprocessedPage(ctx, 0)
	if e != nil || len(pending) != 1 {
		t.Fatal("processing queue wrong", e, pending)
	}
	triage := Triage{EventID: id, Outcome: "notified", Reason: "new review needs attention", MessageID: "22222222-2222-2222-2222-222222222222", Target: "dm:@tester"}
	if e = s.RecordTriage(ctx, triage); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordTriage(ctx, triage); e != nil {
		t.Fatal("triage replay", e)
	}
	triage.Reason = "different"
	if e = s.RecordTriage(ctx, triage); e == nil {
		t.Fatal("overwrite accepted")
	}
	reopened := openTestStore(t)
	pending, e = reopened.UnprocessedPage(ctx, 0)
	if e != nil || len(pending) != 0 {
		t.Fatal("processed notification replay after restart", e)
	}
	if e = s.ImportLegacy(ctx, cfg, bytes.ReplaceAll(raw, []byte(`"private":false`), []byte(`"private":true`))); e == nil {
		t.Fatal("different/private import accepted")
	}
}

func TestQuietTriageManifestOmitsMessage(t *testing.T) {
	m := (&Service{}).Manifest()
	for _, a := range m["actions"].([]any) {
		action := a.(map[string]any)
		if action["name"] == "record-triage" {
			params := action["parameters"].(map[string]any)
			for _, k := range []string{"message_id", "target"} {
				if params[k].(map[string]any)["required"] != false {
					t.Fatal("quiet action must not require fictional message", k)
				}
			}
			return
		}
	}
	t.Fatal("missing triage action")
}
