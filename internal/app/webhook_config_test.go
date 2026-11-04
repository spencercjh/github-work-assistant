package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebhookConfigSelectsOneSource(t *testing.T) {
	for _, tc := range []struct {
		source          string
		discover, valid bool
	}{
		{"", true, true}, {"polling", true, true}, {"webhook", false, true}, {"webhook", true, false}, {"both", false, false},
	} {
		c := Config{Origin: "https://app.example", RaftAPI: "https://api.raft.build", ClientID: "x", ServerID: "s", ServerSlug: "slug", AgentID: "a", GitHubLogin: "user", Watches: []Watch{{"example/fixture", 1}}, GitHubSource: tc.source, Discover: tc.discover}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, canonical(c), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if (err == nil) != tc.valid || (tc.source == "" && loaded.GitHubSource != "polling") {
			t.Fatalf("source=%q discovery=%v: %v", tc.source, tc.discover, err)
		}
	}
}
