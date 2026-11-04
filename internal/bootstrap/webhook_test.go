package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github-raft-app/internal/app"
)

func TestWebhookInstanceHasPrivateSecretAndExclusiveSource(t *testing.T) {
	o := fixtureOptions(t)
	o.GitHubSource, o.Watches = "webhook", []app.Watch{{Repo: "example/fixture", Number: 1}}
	if err := Write(o); err != nil {
		t.Fatal(err)
	}
	c, err := app.LoadConfig(filepath.Join(o.Dir, "config.json"))
	if err != nil || c.GitHubSource != "webhook" || c.Discover {
		t.Fatal("webhook setup enabled polling discovery", c, err)
	}
	secret, err := app.ReadPrivateFile(c.PrivateFile("github-webhook-secret"))
	if err != nil || len(secret) != 64 {
		t.Fatal("missing private webhook signing secret", err)
	}
	config, _ := os.ReadFile(filepath.Join(o.Dir, "config.json"))
	if strings.Contains(string(config), string(secret)) {
		t.Fatal("secret was embedded in public configuration")
	}
	o.Dir = filepath.Join(t.TempDir(), "polling")
	o.GitHubSource = ""
	if err = Write(o); err != nil {
		t.Fatal(err)
	}
	c, err = app.LoadConfig(filepath.Join(o.Dir, "config.json"))
	if err != nil || c.GitHubSource != "polling" || !c.Discover {
		t.Fatal("default source changed", c, err)
	}
}
