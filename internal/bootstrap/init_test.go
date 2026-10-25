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
	return Options{Database: "mysql", Dir: filepath.Join(base, "instance with ' % $ characters"), Executable: executable, Token: []byte("sk_agent_local_fixture"), Identity: identity, GitHubLogin: "fixture-user", Recipient: "@worker", Notify: "@owner", RaftAPI: "https://api.raft.build", Port: 13306}
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

func TestSharedChannelSetupAndClaimInstructions(t *testing.T) {
	o := fixtureOptions(t)
	o.Recipient, o.TaskChannel = "", "#开发工作"
	if err := Write(o); err != nil {
		t.Fatal(err)
	}
	c, err := app.LoadConfig(filepath.Join(o.Dir, "config.json"))
	if err != nil || !c.SharedTasks() || c.DeliveryTarget != o.TaskChannel {
		t.Fatal("shared channel not configured", err)
	}
	guide, err := os.ReadFile(filepath.Join(o.Dir, "EXECUTOR.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"raft task claim", "verify-claim", "静默结束", "不重试争抢", "网络失败", "--executor", "频道成员管理"} {
		if !strings.Contains(string(guide), phrase) {
			t.Fatal("missing execution instruction", phrase)
		}
	}
	for _, channel := range []string{"#work:12345678", "dm:@worker~agent", "work", "#work\n@intruder"} {
		o := fixtureOptions(t)
		o.Recipient, o.TaskChannel = "", channel
		if err := Write(o); err == nil {
			t.Fatal("invalid shared channel accepted", channel)
		}
	}
	o = fixtureOptions(t)
	o.TaskChannel = "#work"
	if err := Write(o); err == nil {
		t.Fatal("both delivery modes accepted")
	}
}

func TestSelectableStorageDoesNotProvisionUnwantedDatabase(t *testing.T) {
	for _, backend := range []string{"sqlite", "mysql", "seekdb", "tidb", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			o := fixtureOptions(t)
			o.Database = backend
			if backend == "sqlite" {
				o.SQLitePath = filepath.Join(t.TempDir(), "custom.sqlite")
			} else {
				o.DSN = []byte("private operator-provided connection")
			}
			if err := Write(o); err != nil {
				t.Fatal(err)
			}
			c, err := app.LoadConfig(filepath.Join(o.Dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if c.DatabaseDriver != backend {
				t.Fatal("driver selection lost")
			}
			for _, name := range []string{"database.yml", "private/database.env"} {
				if _, err := os.Stat(filepath.Join(o.Dir, name)); !os.IsNotExist(err) {
					t.Fatal("unexpected local server provisioned", name)
				}
			}
			dsn, err := app.ReadPrivateFile(c.PrivateFile("dsn"))
			if err != nil {
				t.Fatal(err)
			}
			if backend == "sqlite" {
				if string(dsn) != o.SQLitePath {
					t.Fatal("file location changed")
				}
				if _, err := os.Stat(o.SQLitePath); !os.IsNotExist(err) {
					t.Fatal("init mutated the database")
				}
			} else if string(dsn) != string(o.DSN) {
				t.Fatal("external connection changed")
			}
		})
	}
	o := fixtureOptions(t)
	o.Database = "postgres"
	if err := Write(o); err == nil {
		t.Fatal("external database accepted without connection")
	}
	if _, err := os.Stat(o.Dir); !os.IsNotExist(err) {
		t.Fatal("failed init left an instance")
	}
	o = fixtureOptions(t)
	o.Database = "unsupported"
	if err := Write(o); err == nil {
		t.Fatal("unsupported driver accepted")
	}
}
