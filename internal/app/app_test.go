package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRestrictsOriginsAndWatches(t *testing.T) {
	c := Config{Origin: "https://app.example", RaftAPI: "https://api.raft.build", ClientID: "x", ServerID: "s", ServerSlug: "slug", AgentID: "a", GitHubLogin: "user", Watches: []Watch{{"Project-HAMi/HAMi", 3259}}}
	path := filepath.Join(t.TempDir(), "config.json")
	check := func(want bool) {
		b, _ := json.Marshal(c)
		os.WriteFile(path, b, 0600)
		_, e := LoadConfig(path)
		if (e == nil) != want {
			t.Fatalf("expected valid=%v: %v", want, e)
		}
	}
	check(true)
	c.Origin = "http://evil.example"
	check(false)
	c.Origin = "http://127.0.0.1:4185"
	c.LocalHTTP = true
	check(true)
	c.RaftAPI = "http://127.0.0.1:9999"
	check(false)
	c.RaftAPI = c.Origin
	check(false) // Local App HTTP must never permit a plaintext Raft credential.
	c.RaftAPI = "https://api.raft.build"
	c.Watches[0].Repo = "x/../secrets"
	check(false)
}
func TestRaftIdentityBindingAndRedirect(t *testing.T) {
	cfg := Config{ClientID: "app", ServerID: "server", AgentID: "agent"}
	identity := Identity{Subject: "agent", Type: "agent", ServerID: "server", ClientID: "app"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing bearer")
		}
		json.NewEncoder(w).Encode(identity)
	}))
	defer upstream.Close()
	cfg.RaftAPI = upstream.URL
	r := Raft{Config: cfg, HTTP: SafeHTTP()}
	if _, e := r.User(context.Background(), "test"); e != nil {
		t.Fatal(e)
	}
	identity.Type = "human"
	if _, e := r.User(context.Background(), "test"); e == nil {
		t.Fatal("human accepted by agent-only callback")
	}
	identity.Type = "agent"
	identity.ServerID = "another-server"
	if _, e := r.User(context.Background(), "test"); e == nil {
		t.Fatal("cross-server identity accepted")
	}
	identity.ServerID = "server"
	identity.ClientID = "other-app"
	if _, e := r.User(context.Background(), "test"); e == nil {
		t.Fatal("other-app token accepted")
	}
	redirected := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer evil.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, evil.URL, 302) }))
	defer redirect.Close()
	r.Config.RaftAPI = redirect.URL
	if _, e := r.User(context.Background(), "test"); e == nil || redirected {
		t.Fatal("credential redirect not rejected")
	}
}
func TestNormalizationKeepsEditsAndDropsUserNoise(t *testing.T) {
	a, _ := discussion([]byte(`[[{"id":1,"body":"old","user":{"login":"u","followers":1}}]]`))
	b, _ := discussion([]byte(`[[{"id":1,"body":"old","user":{"login":"u","followers":2}}]]`))
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(aa) != string(bb) {
		t.Fatal("account noise changes evidence")
	}
	c, _ := discussion([]byte(`[[{"id":1,"body":"new","user":{"login":"u"}}],[{"id":2,"body":"second"}]]`))
	if len(c) != 2 || string(c[0]["body"]) != `"new"` {
		t.Fatal("pagination or edit lost")
	}
}
func TestManifestPublicContract(t *testing.T) {
	s := Service{Config: Config{Origin: "https://app.example", ClientID: "ghraft"}}
	m := s.Manifest()
	if m["schema"] != "raft-agent-manifest.v0" {
		t.Fatal("wrong public manifest schema")
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "secret") {
		t.Fatal("manifest contains secret")
	}
	for _, action := range m["actions"].([]any) {
		name := action.(map[string]any)["name"].(string)
		for _, forbidden := range []string{"push", "merge", "comment", "review", "shell"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("manifest exposes GitHub write or execution action: %s", name)
			}
		}
	}
}
