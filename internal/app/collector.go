package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Objects map[string]map[string]any
type Snapshot struct {
	Key            string  `json:"resource_key"`
	Classification string  `json:"classification"`
	Objects        Objects `json:"objects"`
}

func canonical(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
func fields(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func str(v any) string { s, _ := v.(string); return s }
func names(v any, k string) []string {
	out := []string{}
	a, _ := v.([]any)
	for _, x := range a {
		out = append(out, str(obj(x)[k]))
	}
	sort.Strings(out)
	return out
}
func (g GH) get(ctx context.Context, p string) (map[string]any, error) {
	b, e := g.api(ctx, p, false)
	if e != nil {
		return nil, e
	}
	var m map[string]any
	e = json.Unmarshal(b, &m)
	return m, e
}
func (g GH) pages(ctx context.Context, p, field string) ([]map[string]any, error) {
	b, e := g.api(ctx, p, true)
	if e != nil {
		return nil, e
	}
	var pages []json.RawMessage
	if e = json.Unmarshal(b, &pages); e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, p := range pages {
		if field != "" {
			var m map[string]json.RawMessage
			if e = json.Unmarshal(p, &m); e != nil {
				return nil, e
			}
			p = m[field]
		}
		var a []map[string]any
		if e = json.Unmarshal(p, &a); e != nil {
			return nil, e
		}
		out = append(out, a...)
	}
	return out, nil
}
func normalizeRow(kind string, row map[string]any) (string, map[string]any, bool) {
	n := fields(row, "id", "node_id", "event", "body", "state", "name", "status", "conclusion", "context", "description", "html_url", "details_url", "target_url", "created_at", "updated_at", "submitted_at", "completed_at", "commit_id", "path", "line", "in_reply_to_id", "dismissed_review", "label", "rename", "source", "assignee", "requested_reviewer", "requested_team", "review_requester")
	n["actor"] = nil
	for _, k := range []string{"user", "actor", "creator"} {
		if u := obj(row[k]); len(u) > 0 {
			n["actor"] = u["login"]
			break
		}
	}
	if kind == "timeline" && row["event"] == "cross-referenced" {
		source := obj(row["source"])
		issue := obj(source["issue"])
		repo := obj(issue["repository"])
		// Keep cross-repository private references out of stored context, even when
		// the parent is private: source permissions may change independently.
		if p, ok := repo["private"].(bool); !ok || p {
			return "", nil, false
		}
		n["source"] = map[string]any{"type": source["type"], "issue": fields(issue, "id", "node_id", "number", "html_url", "url")}
	}
	id := ""
	if v, ok := row["id"]; ok && v != nil {
		if n, ok := v.(float64); ok {
			id = strconv.FormatFloat(n, 'f', -1, 64)
		} else {
			id = fmt.Sprint(v)
		}
	}
	if id == "" {
		id = str(row["node_id"])
	}
	if id == "" {
		id = hash(canonical(n))
	}
	return kind + ":" + id, n, true
}
func (g GH) Collect(ctx context.Context, w Watch) ([]byte, error) {
	return g.collect(ctx, w, false)
}

// Merge commit evidence belongs to fork verification, not ordinary PR changes.
func (g GH) collect(ctx context.Context, w Watch, mergeEvidence bool) ([]byte, error) {
	classification, e := g.RepositoryClassification(ctx, w.Repo)
	if e != nil {
		return nil, e
	}
	prefix := "repos/" + w.Repo
	ib := fmt.Sprintf("%s/issues/%d", prefix, w.Number)
	issue, e := g.get(ctx, ib)
	if e != nil {
		return nil, e
	}
	io := map[string]any{}
	for _, k := range []string{"id", "number", "title", "body", "state", "state_reason", "html_url", "created_at", "closed_at"} {
		io[k] = issue[k]
	}
	io["author"] = obj(issue["user"])["login"]
	io["assignees"] = names(issue["assignees"], "login")
	io["labels"] = names(issue["labels"], "name")
	objects := Objects{"issue": io}
	type stream struct{ kind, path, field string }
	streams := []stream{{"comment", ib + "/comments?per_page=100", ""}, {"timeline", ib + "/timeline?per_page=100", ""}}
	if _, ok := issue["pull_request"]; ok {
		pb := fmt.Sprintf("%s/pulls/%d", prefix, w.Number)
		pull, e := g.get(ctx, pb)
		if e != nil {
			return nil, e
		}
		po := map[string]any{}
		for _, k := range []string{"state", "draft", "merged", "merged_at", "html_url"} {
			po[k] = pull[k]
		}
		sha := str(obj(pull["head"])["sha"])
		if !shaRE.MatchString(sha) {
			return nil, errors.New("invalid head SHA")
		}
		baseSHA := str(obj(pull["base"])["sha"])
		if !shaRE.MatchString(baseSHA) {
			return nil, errors.New("invalid base SHA")
		}
		if mergeEvidence && pull["merged"] == true {
			po["merge_commit_sha"] = pull["merge_commit_sha"]
		}
		po["head"] = sha
		po["base"] = obj(pull["base"])["ref"]
		po["base_sha"] = baseSHA
		po["head_ref"] = obj(pull["head"])["ref"]
		po["head_repo"] = obj(obj(pull["head"])["repo"])["full_name"]
		po["mergeable"] = pull["mergeable"]
		po["mergeable_state"] = pull["mergeable_state"]
		po["conflict_status"] = "unknown"
		if mergeable, known := pull["mergeable"].(bool); known {
			if mergeable {
				po["conflict_status"] = "clean"
			} else {
				po["conflict_status"] = "conflicting"
			}
		}
		po["requested_reviewers"] = names(pull["requested_reviewers"], "login")
		po["requested_teams"] = names(pull["requested_teams"], "slug")
		objects["pull"] = po
		streams = append(streams, stream{"review", pb + "/reviews?per_page=100", ""}, stream{"review_comment", pb + "/comments?per_page=100", ""}, stream{"check", prefix + "/commits/" + sha + "/check-runs?per_page=100", "check_runs"}, stream{"status", prefix + "/commits/" + sha + "/statuses?per_page=100", ""})
	}
	for _, s := range streams {
		rows, e := g.pages(ctx, s.path, s.field)
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			k, n, allowed := normalizeRow(s.kind, row)
			if allowed {
				objects[k] = n
			}
		}
	}
	// A PR may move while reviews/checks are being fetched. Do not persist mixed revisions.
	if pull, ok := objects["pull"]; ok {
		latest, err := g.get(ctx, fmt.Sprintf("%s/pulls/%d", prefix, w.Number))
		if err != nil {
			return nil, err
		}
		if str(obj(latest["head"])["sha"]) != str(pull["head"]) || str(obj(latest["base"])["sha"]) != str(pull["base_sha"]) || latest["merged"] != pull["merged"] || latest["state"] != pull["state"] {
			return nil, errors.New("PR revisions changed during collection; retry required")
		}
	}
	return canonical(Snapshot{w.Key(), classification, objects}), nil
}

var notificationPath = regexp.MustCompile(`^/repos/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:issues|pulls)/([0-9]+)$`)

func (g GH) notificationWatch(row map[string]any) (Watch, bool) {
	subject := obj(row["subject"])
	if subject["type"] != "Issue" && subject["type"] != "PullRequest" {
		return Watch{}, false
	}
	u, e := url.Parse(str(subject["url"]))
	if e != nil || u.Scheme != "https" || u.Host != "api.github.com" || u.User != nil || u.RawQuery != "" {
		return Watch{}, false
	}
	m := notificationPath.FindStringSubmatch(u.Path)
	if m == nil || !repoRE.MatchString(m[1]) || strings.Contains(m[1], "..") {
		return Watch{}, false
	}
	n, _ := strconv.Atoi(m[2])
	w := Watch{m[1], n}
	private, ok := obj(row["repository"])["private"].(bool)
	if !ok || n < 1 || (private && !privateRepositoryAllowed(g.PrivateRepositories, w.Repo)) {
		return Watch{}, false
	}
	if name := str(obj(row["repository"])["full_name"]); name != "" && !strings.EqualFold(name, w.Repo) {
		return Watch{}, false
	}
	if private && g.PrivateNotificationsSince != "" {
		since, err := time.Parse(time.RFC3339, g.PrivateNotificationsSince)
		updated, updatedErr := time.Parse(time.RFC3339, str(row["updated_at"]))
		if err != nil || updatedErr != nil || updated.Before(since) {
			return Watch{}, false
		}
	}
	return w, true
}
func (g GH) Discover(ctx context.Context, since string) ([]Watch, string, error) {
	// Notifications supports at most 50 items per page; g.pages follows all pages.
	rows, e := g.pages(ctx, "notifications?all=true&per_page=50&since="+url.QueryEscape(since), "")
	if e != nil {
		return nil, "", e
	}
	latest, _ := time.Parse(time.RFC3339, since)
	seen := map[string]bool{}
	out := []Watch{}
	for _, row := range rows {
		if t, e := time.Parse(time.RFC3339, str(row["updated_at"])); e == nil && t.After(latest) {
			latest = t
		}
		w, ok := g.notificationWatch(row)
		if ok && !seen[w.Key()] {
			out = append(out, w)
			seen[w.Key()] = true
		}
	}
	next := latest.Add(-2 * time.Minute).Format(time.RFC3339)
	if next < since {
		next = since
	}
	return out, next, nil
}
