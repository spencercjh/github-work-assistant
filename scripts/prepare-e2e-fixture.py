#!/usr/bin/env python3
"""Create an offline Git/GitHub fixture for a real Raft agent to exercise the CLI.

No git commit, GitHub API or Raft operation is performed. Synthetic Git objects
are test data. The generated gh executable rejects everything except fixture GETs.
"""
import argparse
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    parser.add_argument("--task-target", default="#local-test")
    args = parser.parse_args()
    root = Path(args.output).resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    repo = root / "fixture.git"
    repo.mkdir()

    def git(*argv, data=""):
        return subprocess.check_output(["git", "-C", str(repo), *argv], input=data.encode(), stderr=subprocess.PIPE).decode().strip()

    git("init", "--bare")
    git("remote", "add", "origin", "https://github.com/example/fixture.git")

    def commit(parent, source, offset, feature=False):
        files = {
            "calculator.py": source,
            "test_base.py": f"import unittest\nfrom calculator import score\nclass OffsetTest(unittest.TestCase):\n    def test_offset(self):\n        self.assertEqual(score(0), {offset})\n",
        }
        if feature:
            files["test_feature.py"] = "import unittest\nfrom calculator import score\nclass FeatureTest(unittest.TestCase):\n    def test_scale(self):\n        self.assertEqual(score(3) - score(0), 6)\n"
        rows = []
        for name, content in sorted(files.items()):
            blob = git("hash-object", "-w", "--stdin", data=content)
            rows.append(f"100644 blob {blob}\t{name}\n")
        tree = git("mktree", data="".join(rows))
        body = f"tree {tree}\n" + (f"parent {parent}\n" if parent else "")
        body += "author Test Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Test Fixture <fixture@example.invalid> 1700000000 +0000\n\nSynthetic local fixture\n"
        return git("hash-object", "-w", "-t", "commit", "--stdin", data=body)

    ancestor = commit("", "def score(x):\n    return x + 1\n", 1)
    head = commit(ancestor, "def score(x):\n    return x * 2 + 1\n", 1, True)
    base = commit(ancestor, "def score(x):\n    return x + 2\n", 2)
    git("update-ref", "refs/heads/feature", head)
    git("update-ref", "refs/heads/main", base)
    response = {
        "user": {"login": "fixture-user"},
        "repos/example/fixture": {"full_name": "example/fixture", "private": False},
        "repos/example/fixture/issues/1": {"id": 1, "number": 1, "title": "Preserve base offset and PR scaling", "state": "open", "pull_request": {}, "body": "Local fixture; no external repository is contacted."},
        "repos/example/fixture/pulls/1": {"state": "open", "draft": False, "merged": False, "mergeable": False, "mergeable_state": "dirty", "head": {"sha": head, "ref": "feature", "repo": {"full_name": "example/fixture"}}, "base": {"sha": base, "ref": "main"}},
    }
    for suffix in ["issues/1/comments", "issues/1/timeline", "pulls/1/reviews", "pulls/1/comments", f"commits/{head}/statuses"]:
        response[f"repos/example/fixture/{suffix}?per_page=100"] = [[]]
    response[f"repos/example/fixture/commits/{head}/check-runs?per_page=100"] = [{"check_runs": []}]
    (root / "responses.json").write_text(json.dumps(response, indent=2) + "\n")
    (root / "bin").mkdir()
    gh = root / "bin" / "gh"
    gh.write_text('''#!/usr/bin/env python3
import json, pathlib, sys
root=pathlib.Path(__file__).resolve().parents[1]
a=sys.argv[1:]
if len(a)<6 or a[:5]!=['api','--hostname','github.com','--method','GET'] or a[6:] not in ([],['--paginate','--slurp']):
    sys.exit('fixture rejected a non-read-only command')
with (root/'requests.jsonl').open('a') as f: f.write(json.dumps(a)+'\\n')
data=json.loads((root/'responses.json').read_text())
if a[5] not in data: sys.exit('fixture endpoint is not configured')
print(json.dumps(data[a[5]]))
''')
    gh.chmod(0o700)
    (root / "guide.md").write_text("# Local conflict acceptance\n\nPreserve the base branch offset of 2 and the PR's doubling behavior. Run both unittest cases in the prepared worktree. Deliver evidence in the bound Raft task. No commits or remote writes.\n")
    config = {
        "listen": "127.0.0.1:4189", "origin": "http://127.0.0.1:4189", "local_http": True,
        "raft_api": "https://api.raft.build", "client_id": "offline-fixture", "server_id": "offline-server", "agent_id": "offline-agent", "github_login": "fixture-user",
        "poll_seconds": 120, "lookback_hours": 6, "discover_notifications": False, "notify_target": "dm:@fixture-human",
        "watches": [{"repo": "example/fixture", "number": 1}], "workspace_root": "worktrees",
        "repositories": [{"repo": "example/fixture", "goal": "Resolve and test the local conflict fixture", "mode": "local-edit", "guide_file": "guide.md", "task_target": args.task_target, "test_commands": [["python3", "-m", "unittest", "discover", "-v"]]}],
    }
    (root / "config.json").write_text(json.dumps(config, indent=2) + "\n")
    print(json.dumps({"root": str(root), "config": str(root / "config.json"), "repo": str(repo), "head": head, "base": base, "gh_path": str(root / "bin")}, indent=2))


if __name__ == "__main__":
    main()
