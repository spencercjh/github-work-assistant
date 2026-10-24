package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github-raft-app/internal/app"
)

func fixtureOptions(t *testing.T) Options {
	t.Helper()
	base := t.TempDir()
	executable := filepath.Join(base, "bin with ' quote")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	identity := app.ExternalIdentity{}
	identity.Agent.ID, identity.Agent.Name, identity.Agent.External = "sender-id", "collector", true
	identity.Server.ID = "server-id"
	return Options{Dir: filepath.Join(base, "instance with ' % $ characters"), Executable: executable, Token: []byte("sk_agent_local_fixture"), Identity: identity, GitHubLogin: "fixture-user", Recipient: "@worker", Notify: "@owner", RaftAPI: "https://api.raft.build", Port: 13306}
}

func TestInstanceIsPrivateCompleteAndNeverOverwritten(t *testing.T) {
	o := fixtureOptions(t)
	if err := Write(o); err != nil {
		t.Fatal(err)
	}
	c, err := app.LoadConfig(filepath.Join(o.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RaftMode != "external" || c.ExternalAgent != "sender-id" || c.DeliveryTarget != "dm:@worker~agent" || !c.Discover || len(c.Watches) != 0 {
		t.Fatalf("wrong configuration: %+v", c)
	}
	for _, name := range []string{"private", "private/key", "private/raft-token", "private/dsn", "private/database.env", "config.json"} {
		info, err := os.Stat(filepath.Join(o.Dir, name))
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("file is not private: %s %v", name, err)
		}
	}
	key, _ := os.ReadFile(c.PrivateFile("key"))
	if len(strings.TrimSpace(string(key))) != 64 {
		t.Fatal("invalid encryption key")
	}
	cmd := exec.Command(filepath.Join(o.Dir, "run"), "work-context", "--event", "some value")
	cmd.Dir = t.TempDir()
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	expected := "-config\n" + filepath.Join(o.Dir, "config.json") + "\nwork-context\n--event\nsome value\n"
	if string(b) != expected {
		t.Fatalf("wrapper changed arguments: %q", b)
	}
	if err = Write(o); err == nil {
		t.Fatal("existing instance overwritten")
	}
	after, _ := os.ReadFile(c.PrivateFile("key"))
	if string(key) != string(after) {
		t.Fatal("encryption key rotated unexpectedly")
	}
	compose, _ := os.ReadFile(filepath.Join(o.Dir, "database.yml"))
	if !strings.Contains(string(compose), "127.0.0.1:13306:3306") || strings.Contains(string(compose), "MYSQL_PASSWORD=") {
		t.Fatal("database binding or secret separation lost")
	}
	if tool, err := exec.LookPath("systemd-analyze"); err == nil {
		units, _ := filepath.Glob(filepath.Join(o.Dir, "ghraft-*.service"))
		cmd := exec.Command(tool, "--user", "verify", "--man=no", "--generators=no", units[0])
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated service is invalid: %v %s", err, out)
		}
	}
}

func TestInvalidSetupLeavesNoPartialInstance(t *testing.T) {
	o := fixtureOptions(t)
	o.Watches = []app.Watch{{Repo: "bad/../../repo", Number: 1}}
	if err := Write(o); err == nil {
		t.Fatal("invalid watch accepted")
	}
	if _, err := os.Stat(o.Dir); !os.IsNotExist(err) {
		t.Fatal("partial instance published")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(o.Dir), ".ghraft-init-*"))
	if len(matches) != 0 {
		t.Fatal("partial secret files retained")
	}
	o = fixtureOptions(t)
	o.Recipient = "@collector"
	if err := Write(o); err == nil {
		t.Fatal("self delivery allowed")
	}
	o = fixtureOptions(t)
	o.Notify = "@worker~agent"
	if err := Write(o); err == nil {
		t.Fatal("agent notification handle accepted as human")
	}
}
