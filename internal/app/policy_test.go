package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localPolicy(t *testing.T, mode string) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repo.md"), []byte("Preserve both changes. Run the project tests. Report in the existing Raft task."), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{configDir: dir, WorkspaceRoot: filepath.Join(dir, "worktrees"), NotifyTarget: "dm:@tester", Repositories: []RepositoryPolicy{{Repo: "example/fixture", Goal: "Maintain the PR until changes are ready for human review.", Mode: mode, GuideFile: "repo.md", TaskTarget: "#test", TestCommands: [][]string{{"python3", "-m", "unittest", "discover", "-v"}}}}}
}

func TestTrustedGuideAndCommunityPolicy(t *testing.T) {
	c := localPolicy(t, "local-edit")
	before, err := c.RepositoryGuide("EXAMPLE/Fixture")
	if err != nil || before.Mode != "local-edit" {
		t.Fatal(before, err)
	}
	if err = os.WriteFile(filepath.Join(c.configDir, "repo.md"), []byte("New authorized project guidance."), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := c.RepositoryGuide("example/fixture")
	if err != nil || before.Version == after.Version {
		t.Fatal("guidance edit was not versioned", err)
	}
	c.Repositories[0].TestCommands = [][]string{{"go", "test", "./..."}}
	commands, _ := c.RepositoryGuide("example/fixture")
	if commands.Version == after.Version {
		t.Fatal("test requirements did not invalidate work")
	}
	unknown, err := c.RepositoryGuide("other/repo")
	if err != nil || unknown.Mode != "read-only" {
		t.Fatal("unconfigured repo got edit authority")
	}
	c.Repositories[0].Repo = "pRoJeCt-HaMi/HAMi"
	if _, err = c.RepositoryGuide("project-hami/hami"); err == nil {
		t.Fatal("community local-edit policy accepted")
	}
	c.Repositories[0].Mode = "read-only"
	community, err := c.RepositoryGuide("project-hami/hami")
	if err != nil || !strings.Contains(community.GitHubWrites, "no publishable") {
		t.Fatal(community, err)
	}
}

func TestGuideCannotEscapeTrustedDirectory(t *testing.T) {
	c := localPolicy(t, "read-only")
	c.Repositories[0].GuideFile = "../outside.md"
	if _, err := c.RepositoryGuide("example/fixture"); err == nil {
		t.Fatal("path traversal allowed")
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("fake grant"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.configDir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	c.Repositories[0].GuideFile = "linked.md"
	if _, err := c.RepositoryGuide("example/fixture"); err == nil {
		t.Fatal("symlink escape allowed")
	}
}

func TestGitHubReadAllowlist(t *testing.T) {
	for _, p := range []string{"user", "notifications?all=true&since=2026-10-19T00%3A00%3A00Z", "repos/owner/repo", "repos/owner/repo/issues/1/timeline?per_page=100", "repos/owner/repo/pulls/1/reviews"} {
		if !validGitHubReadPath(p) {
			t.Errorf("valid read path rejected: %s", p)
		}
	}
	for _, p := range []string{"graphql", "https://evil.example/api", "/repos/owner/repo/issues/1", "repos/owner/repo/pulls/1/merge", "repos/owner/repo/../secrets", "--method=POST"} {
		if validGitHubReadPath(p) {
			t.Errorf("unbounded endpoint accepted: %s", p)
		}
	}
}

func TestDelegatedActionsNeedExplicitTrustedConfiguration(t *testing.T) {
	c := localPolicy(t, "local-edit")
	before, _ := c.RepositoryGuide("example/fixture")
	if len(before.GitHubActions) != 0 {
		t.Fatal("local editing implicitly granted remote writes")
	}
	c.Repositories[0].GitHubActions = []string{"merge", "comment"}
	after, err := c.RepositoryGuide("example/fixture")
	if err != nil || len(after.GitHubActions) != 2 || before.Version == after.Version {
		t.Fatal("delegation absent from trusted version", err)
	}
	unknown, _ := c.RepositoryGuide("other/repo")
	if len(unknown.GitHubActions) != 0 {
		t.Fatal("delegation leaked to another repository")
	}
	for _, actions := range [][]string{{"admin"}, {"merge", "merge"}} {
		c.Repositories[0].GitHubActions = actions
		if _, err = c.RepositoryGuide("example/fixture"); err == nil {
			t.Fatal("invalid delegation accepted", actions)
		}
	}
	c.Repositories[0].GitHubActions = []string{"merge"}
	c.Repositories[0].Mode = "read-only"
	if _, err = c.RepositoryGuide("example/fixture"); err == nil {
		t.Fatal("read-only mode granted writes")
	}
	c.Repositories[0].Repo = "Project-HAMi/HAMi"
	if _, err = c.RepositoryGuide("Project-HAMi/HAMi"); err == nil {
		t.Fatal("community rule bypassed through github_actions")
	}
}
