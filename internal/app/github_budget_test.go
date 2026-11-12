package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubResponseHeadersAndPagination(t *testing.T) {
	for _, sep := range []string{"\r\n", "\n"} {
		t.Run(fmt.Sprintf("separator-%q", sep), func(t *testing.T) {
			response := func(status int, headers, body string) string {
				return fmt.Sprintf("HTTP/2.0 %d Fixture%s%s%s%s%s", status, sep, headers, sep, sep, body)
			}
			first := response(200, "Content-Type: application/json", `[{"text":"HTTP/2.0 403\n\nprivate text"}]`)
			second := response(200, "X-Page: two", `[{"id":2}]`)
			b, h, code, err := decodeGitHubResponses([]byte(first+"\n"+second), true)
			var pages [][]map[string]any
			if err != nil || json.Unmarshal(b, &pages) != nil || len(pages) != 2 || len(pages[0]) != 1 || pages[1][0]["id"] != float64(2) || h.Get("X-Page") != "two" || code != 200 {
				t.Fatal("pagination or embedded text corrupted", string(b), code, err)
			}
			limited := response(403, "Retry-After: 123", `{"message":"secondary rate limit"}`)
			b, h, code, err = decodeGitHubResponses([]byte(first+"\n"+limited), true)
			if err != nil || code != 403 || h.Get("Retry-After") != "123" || !strings.Contains(string(b), "secondary") {
				t.Fatal("later page error lost", code, h, err)
			}
			for _, malformed := range []string{"", "not headers", "HTTP/2.0 nope\n\n{}", response(200, "X: y", "broken"), first + "\n" + second} {
				if _, _, _, err = decodeGitHubResponses([]byte(malformed), false); err == nil {
					t.Fatal("invalid single-page response accepted")
				}
			}
		})
	}
}

func TestGitHubRetryDeadlineHeaders(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name             string
		h                http.Header
		minimum, maximum int64
		fallback         bool
	}{
		{"primary reset", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now+180, 10)}}, now + 181, now + 181, false},
		{"secondary seconds", http.Header{"Retry-After": {"120"}}, now + 121, now + 123, false},
		{"secondary date", http.Header{"Retry-After": {time.Unix(now+240, 0).UTC().Format(http.TimeFormat)}}, now + 241, now + 241, false},
		{"later limit wins", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now+180, 10)}, "Retry-After": {"300"}}, now + 301, now + 303, false},
		{"missing", nil, now + 60, now + 62, true},
		{"malformed", http.Header{"Retry-After": {"invalid"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"9223372036854775807"}}, now + 60, now + 62, true},
		{"expired", http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1"}}, now + 60, now + 62, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := rateLimitFromHeaders(tc.h)
			if e.Until < tc.minimum || e.Until > tc.maximum || e.Fallback != tc.fallback || !errors.Is(e, ErrGitHubRateLimited) {
				t.Fatal(e.GitHubPause)
			}
		})
	}
}

func TestGitHubRateLimitSharedAcrossConnectionsAndResumes(t *testing.T) {
	g, path := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	store := freshTestStore(t)
	g.Store = store
	other := g
	other.Store = openTestStore(t)
	log := filepath.Join(t.TempDir(), "requests")
	t.Setenv("GHRAFT_REQUEST_LOG", log)
	reset := time.Now().Add(3 * time.Minute).Unix()
	writeFixture(t, path, map[string]any{"user": map[string]any{
		"_http_status": 403, "_body": map[string]any{"message": "API rate limit exceeded for PRIVATE-DIAGNOSTIC"},
		"_headers": map[string]any{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
	}})
	for _, client := range []GH{g, other, g} {
		err := client.CheckIdentity(t.Context())
		var limited *GitHubRateLimitError
		if !errors.As(err, &limited) || limited.Until != reset+1 || errors.Is(err, ErrGitHubAccessDenied) || strings.Contains(err.Error(), "PRIVATE-DIAGNOSTIC") {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "user\n" {
		t.Fatal("another process retried before reset", string(calls), err)
	}
	writeFixture(t, path, map[string]any{"user": map[string]any{"login": g.Login}})
	if err = store.PutMeta(t.Context(), githubPauseName(g.Login), string(canonical(GitHubPause{Until: time.Now().Unix() - 1}))); err != nil {
		t.Fatal(err)
	}
	if err = other.CheckIdentity(t.Context()); err != nil {
		t.Fatal("expired pause did not resume", err)
	}
	calls, _ = os.ReadFile(log)
	if string(calls) != "user\nuser\n" {
		t.Fatal(string(calls))
	}
}

func TestGitHubSecondaryLimitDoesNotBecomeRevokedAccess(t *testing.T) {
	g, path := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	for _, status := range []int{403, 429} {
		writeFixture(t, path, map[string]any{"user": map[string]any{"_http_status": status, "_headers": map[string]any{"Retry-After": "90"}}})
		var limited *GitHubRateLimitError
		if err := g.CheckIdentity(t.Context()); !errors.As(err, &limited) || limited.Fallback || limited.Until < time.Now().Unix()+90 {
			t.Fatal("retry header ignored", err)
		}
	}
	writeFixture(t, path, map[string]any{"user": map[string]any{"_http_status": 403, "_body": map[string]any{"message": "permission denied"}}})
	if err := g.CheckIdentity(t.Context()); !errors.Is(err, ErrGitHubAccessDenied) || errors.Is(err, ErrGitHubRateLimited) {
		t.Fatal("real permission failure treated as quota", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := g.CheckIdentity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}

func TestGitHubSharedDeadlineOnlyExtendsAndFallbackBacksOff(t *testing.T) {
	s, ctx := freshTestStore(t), t.Context()
	other := openTestStore(t)
	key := githubPauseName("fixture")
	now := time.Now().Unix()
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for i, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsCh <- store.ExtendGitHubPause(ctx, key, GitHubPause{Until: now + int64(120+i*120)})
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() GitHubPause {
		raw, err := s.Meta(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		var p GitHubPause
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if read().Until != now+240 {
		t.Fatal("shorter deadline overwrote longer limit", read())
	}
	if err := s.PutMeta(ctx, key, "{}"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{60, 120, 240, 480, 900, 900} {
		if err := s.ExtendGitHubPause(ctx, key, GitHubPause{Until: now + 60, Fallback: true, Delay: 60}); err != nil {
			t.Fatal(err)
		}
		if read().Delay != want {
			t.Fatal("fallback did not back off", read(), want)
		}
	}
}

type budgetLimitedSource struct{ calls int }

func (g *budgetLimitedSource) CheckIdentity(context.Context) error {
	g.calls++
	return &GitHubRateLimitError{GitHubPause{Until: time.Now().Add(time.Minute).Unix()}}
}
func (*budgetLimitedSource) Collect(context.Context, Watch) ([]byte, error) {
	panic("must not collect while limited")
}

func TestCollectorWaitsForRateDeadlineAndCancels(t *testing.T) {
	g := &budgetLimitedSource{}
	s := &Service{Store: freshTestStore(t), GH: g}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	s.Loop(ctx)
	if g.calls != 1 {
		t.Fatal("loop retried before deadline", g.calls)
	}
}
