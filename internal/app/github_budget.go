package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type GitHubPause struct {
	Until    int64 `json:"until"`
	Fallback bool  `json:"fallback,omitempty"`
	Delay    int64 `json:"delay,omitempty"`
}

type GitHubRateLimitError struct{ GitHubPause }

func (e *GitHubRateLimitError) Error() string {
	return fmt.Sprintf("%s (retry at %s)", ErrGitHubRateLimited, time.Unix(e.Until, 0).UTC().Format(time.RFC3339))
}
func (e *GitHubRateLimitError) Unwrap() error { return ErrGitHubRateLimited }

func rateLimitFromHeaders(h http.Header) *GitHubRateLimitError {
	now := time.Now()
	var until int64
	if h.Get("X-RateLimit-Remaining") == "0" {
		reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
		if reset > now.Unix() && reset <= now.Add(24*time.Hour).Unix() {
			until = reset + 1
		}
	}
	if seconds, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= 86400 {
		until = max(until, now.Unix()+seconds+1)
	} else if date, err := http.ParseTime(h.Get("Retry-After")); err == nil && date.After(now) && !date.After(now.Add(24*time.Hour)) {
		until = max(until, date.Unix()+1)
	}
	pause := GitHubPause{Until: until}
	if until == 0 {
		pause = GitHubPause{Until: now.Unix() + 60, Fallback: true, Delay: 60}
	}
	return &GitHubRateLimitError{pause}
}

func githubPauseName(login string) string {
	return "github-pause:" + hash([]byte(strings.ToLower(login)))
}

// The durable deadline covers the collector and independent CLI processes.
// It contains no response body, credentials, repository names or URLs.
func (g GH) readCommand(ctx context.Context, args []string, input io.Reader) (json.RawMessage, error) {
	key := githubPauseName(g.Login)
	if g.Store != nil {
		raw, err := g.Store.Meta(ctx, key, "{}")
		if err != nil {
			return nil, err
		}
		var pause GitHubPause
		if err = json.Unmarshal([]byte(raw), &pause); err != nil {
			return nil, errors.New("invalid stored GitHub retry deadline")
		}
		if pause.Until > time.Now().Unix() {
			return nil, &GitHubRateLimitError{pause}
		}
	}
	body, err := githubReadCommand(ctx, args, input)
	var limited *GitHubRateLimitError
	if g.Store != nil && errors.As(err, &limited) {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := g.Store.ExtendGitHubPause(saveCtx, key, limited.GitHubPause); e != nil {
			return nil, errors.Join(err, e)
		}
		// Return the winning (possibly longer) deadline to the service loop.
		raw, e := g.Store.Meta(saveCtx, key, "{}")
		if e != nil {
			return nil, errors.Join(err, e)
		}
		var pause GitHubPause
		if e = json.Unmarshal([]byte(raw), &pause); e != nil {
			return nil, errors.Join(err, e)
		}
		return nil, &GitHubRateLimitError{pause}
	}
	return body, err
}

func (s *Store) ExtendGitHubPause(ctx context.Context, key string, next GitHubPause) error {
	tx, err := s.db.BeginWrite(ctx, key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	var old GitHubPause
	err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, key).Scan(&raw)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			return err
		}
	}
	if next.Fallback && old.Fallback && old.Until > time.Now().Add(-5*time.Minute).Unix() {
		next.Delay = min(15*60, max(60, old.Delay*2))
		next.Until = max(next.Until, time.Now().Unix()+next.Delay)
	}
	if old.Until > next.Until {
		next = old
	}
	if err = tx.Upsert(ctx, &metaRecord{key, string(canonical(next))}, []string{"name"}, []string{"value"}); err != nil {
		return err
	}
	return tx.Commit()
}

// gh --include emits a header block for every page. Decode one JSON value at a
// time, so embedded text (including "HTTP/...") never acts as a page separator.
func decodeGitHubResponses(raw []byte, paginated bool) (json.RawMessage, http.Header, int, error) {
	var pages []json.RawMessage
	var headers http.Header
	status := 0
	for len(bytes.TrimSpace(raw)) > 0 {
		raw = bytes.TrimSpace(raw)
		end, width := bytes.Index(raw, []byte("\r\n\r\n")), 4
		if end < 0 {
			end, width = bytes.Index(raw, []byte("\n\n")), 2
		}
		if end < 0 || !bytes.HasPrefix(raw, []byte("HTTP/")) {
			return nil, headers, status, errors.New("invalid GitHub response headers")
		}
		lines := strings.Split(string(raw[:end]), "\n")
		parts := strings.Fields(lines[0])
		if len(parts) < 2 {
			return nil, headers, status, errors.New("invalid GitHub response status")
		}
		var err error
		status, err = strconv.Atoi(parts[1])
		if err != nil || status < 100 || status > 599 {
			return nil, headers, status, errors.New("invalid GitHub response status")
		}
		headers = make(http.Header)
		for _, line := range lines[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok {
				headers.Add(name, strings.TrimSpace(value))
			}
		}
		raw = raw[end+width:]
		decoder := json.NewDecoder(bytes.NewReader(raw))
		var page json.RawMessage
		if err = decoder.Decode(&page); err != nil {
			return nil, headers, status, errors.New("invalid GitHub JSON response")
		}
		raw = raw[decoder.InputOffset():]
		if status >= 400 {
			return page, headers, status, nil
		}
		pages = append(pages, page)
	}
	if len(pages) == 0 || (!paginated && len(pages) != 1) {
		return nil, headers, status, errors.New("invalid GitHub response page count")
	}
	if paginated {
		return canonical(pages), headers, status, nil
	}
	return pages[0], headers, status, nil
}
