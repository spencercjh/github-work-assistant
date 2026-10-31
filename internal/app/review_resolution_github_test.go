package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewQueryPage(after string, more bool, threads ...reviewThread) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{
		"nameWithOwner": "example/fixture",
		"pullRequest": map[string]any{
			"number": 1, "state": "OPEN", "headRefOid": strings.Repeat("a", 40), "baseRefOid": strings.Repeat("b", 40), "headRefName": "ghraft/sync/fixture/round", "baseRefName": "main",
			"repository": map[string]string{"nameWithOwner": "example/fixture"}, "headRepository": map[string]string{"nameWithOwner": "example/fixture"},
			"reviewThreads": map[string]any{"nodes": threads, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": after}},
		},
	}}}
}

func fakeReviewGH(t *testing.T) (GH, string, string) {
	t.Helper()
	g, fixture := fakeGH(t, strings.Repeat("a", 40), strings.Repeat("b", 40))
	dir := filepath.Dir(fixture)
	if err := os.Rename(filepath.Join(dir, "gh"), filepath.Join(dir, "gh-rest")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "query.txt"), []byte(forkReviewThreadQuery), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env python3
import json, os, sys
root=os.path.dirname(__file__)
a=sys.argv[1:]
if a[:5]==['api','--hostname','github.com','--method','GET']:
    os.execv(root+'/gh-rest',[root+'/gh-rest']+a)
if a!=['api','--hostname','github.com','--method','POST','graphql','--input','-']:
    sys.exit('unexpected gh command')
b=json.load(sys.stdin)
with open(root+'/query.txt') as f: expected=f.read()
if b.get('query')!=expected or set(b)!=set(['query','variables']):
    sys.exit('arbitrary query or mutation rejected')
v=b['variables']
if set(v)!=set(['owner','name','number','after']) or (v['owner'],v['name'],v['number'])!=('example','fixture',1):
    sys.exit('unexpected scope')
p='graphql:'+(v['after'] or 'first')
with open(os.environ['GHRAFT_REQUEST_LOG'],'a') as f: f.write(p+'\n')
with open(os.environ['GHRAFT_FIXTURE']) as f: data=json.load(f)
if p not in data: sys.exit('unconfigured query')
if '_raw' in data[p]: print(data[p]['_raw'])
else: print(json.dumps(data[p]))
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "requests.log")
	t.Setenv("GHRAFT_REQUEST_LOG", log)
	return g, fixture, log
}

func TestReviewResolutionGitHubScopedPagination(t *testing.T) {
	g, path, log := fakeReviewGH(t)
	data := readFixture(t, path)
	resolved := false
	data["graphql:first"] = reviewQueryPage("page2", true, reviewThread{ID: "PRRT_other", IsResolved: &resolved})
	data["graphql:page2"] = reviewQueryPage("", false, reviewThread{ID: "PRRT_wanted", IsResolved: &resolved})
	writeFixture(t, path, data)
	pr, thread, err := g.ReviewThread(t.Context(), "example/fixture", 1, "PRRT_wanted")
	if err != nil || thread.ID != "PRRT_wanted" || thread.IsResolved == nil || *thread.IsResolved || pr.Head != strings.Repeat("a", 40) {
		t.Fatal(pr, thread, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "repos/example/fixture\ngraphql:first\ngraphql:page2\n" {
		t.Fatal("wrong query order", string(calls), err)
	}
	// Permission is freshly rechecked before any thread page, including a known ID.
	data["repos/example/fixture"] = map[string]any{"_http_status": 404}
	writeFixture(t, path, data)
	if _, _, err = g.ReviewThread(t.Context(), "example/fixture", 1, "PRRT_wanted"); !errors.Is(err, ErrGitHubAccessDenied) {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(log)
	if strings.Count(string(calls), "graphql:") != 2 {
		t.Fatal("thread read after access revocation")
	}
}

func TestReviewResolutionGitHubRejectsIncompleteOrForeignData(t *testing.T) {
	unresolved := false
	for name, page := range map[string]any{
		"partial errors":     map[string]any{"data": reviewQueryPage("", false, reviewThread{ID: "PRRT_wanted", IsResolved: &unresolved})["data"], "errors": []any{map[string]any{"message": "sensitive error content"}}},
		"null data":          map[string]any{"data": nil},
		"malformed":          map[string]any{"_raw": "{"},
		"not in PR":          reviewQueryPage("", false, reviewThread{ID: "PRRT_other", IsResolved: &unresolved}),
		"unknown resolution": reviewQueryPage("", false, reviewThread{ID: "PRRT_wanted"}),
		"missing cursor":     reviewQueryPage("", true),
		"cycle":              reviewQueryPage("first", true),
		"foreign repo":       map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "other/private"}}},
	} {
		t.Run(name, func(t *testing.T) {
			g, path, _ := fakeReviewGH(t)
			data := readFixture(t, path)
			data["graphql:first"] = page
			writeFixture(t, path, data)
			if _, _, err := g.ReviewThread(t.Context(), "example/fixture", 1, "PRRT_wanted"); err == nil || strings.Contains(err.Error(), "sensitive error content") {
				t.Fatal("accepted invalid/partial response or exposed error", err)
			}
		})
	}
	t.Run("private not allowed", func(t *testing.T) {
		g, path, log := fakeReviewGH(t)
		data := readFixture(t, path)
		obj(data["repos/example/fixture"])["private"] = true
		writeFixture(t, path, data)
		if _, _, err := g.ReviewThread(t.Context(), "example/fixture", 1, "PRRT_wanted"); !errors.Is(err, ErrRepositoryNotAllowed) {
			t.Fatal(err)
		}
		calls, _ := os.ReadFile(log)
		if strings.Contains(string(calls), "graphql:") {
			t.Fatal("private data read outside allowlist")
		}
	})
}
