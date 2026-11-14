"""Workflow boundaries using isolated fake CLI responses; no live credentials."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "skills/upstream-sync/scripts/sync_work.py"
spec = importlib.util.spec_from_file_location("sync_work", SCRIPT)
work = importlib.util.module_from_spec(spec)
spec.loader.exec_module(work)

PROJECT = {"fork": "example", "target": "owner/fork", "upstream": "upstream/source",
           "target_branch": "main", "upstream_branch": "main", "mode": "upstream-only"}
HEAD, BASE, OLD_BASE = "a" * 40, "b" * 40, "c" * 40


def context():
    return {
        "current_event_id": "event-1", "current_version": "version-1", "stale": False,
        "event": {"event_id": "event-1", "resource_key": "fork:example", "state": "accepted"},
        "guide": {"version": "guide-1", "fork_sync_mode": "", "github_actions": ["merge"]},
        "fork": {"phase": "working", "batch_id": "batch-1", "round": "round-1", "revision": 1,
                 "initial_target_sha": OLD_BASE, "validated_head": HEAD, "validated_base": OLD_BASE,
                 "validation": {"id": "proof-1", "state": "passed"}, "wake": 0},
        "snapshot": {"objects": {"fork": {"id": "example", "target_repo": "owner/fork",
                     "upstream_repo": "upstream/source", "target_branch": "main", "upstream_branch": "main",
                     "target_sha": BASE, "upstream_sha": HEAD, "remote_head": HEAD, "wake": 0},
                     "comment:1": {"body": "Please verify current behavior"}}},
        "collaboration": {"revision": 1, "items": [{"kind": "review", "executors": ["reviewer"],
                           "task": {"messageId": "task-1"}, "link": {"version": "version-1"}}]},
        "processed_history": [],
    }


def session():
    return {"schema": 1, "project": copy.deepcopy(PROJECT), "task": "task-1", "executor": "reviewer",
            "context": context()}


def wake_event(current):
    before = copy.deepcopy(current["snapshot"]["objects"]["fork"])
    after = copy.deepcopy(before)
    after["wake"] += 1
    current["event"].update(event_id="event-2", payload={
        "event_id": "event-2", "first_observation": False, "previous_version": "version-1",
        "changes": [{"object": "fork", "kind": "changed", "before": before, "after": after}],
    })
    current["current_event_id"] = "event-2"


class FakeRunner:
    def __init__(self, current=None, failures=0):
        self.current = current or context()
        self.calls = []
        self.failures = failures
        self.claim = True
        self.error = work.STALE

    def __call__(self, command, args):
        self.calls.append((command, args))
        if command == "verify-claim":
            if not self.claim:
                raise work.WorkflowError("ownership changed")
            return {"execute": True}
        if command in ("work-context", "refresh-work-context"):
            return copy.deepcopy(self.current)
        if command in work.REPORTS and self.failures:
            self.failures -= 1
            raise work.CommandError(command, self.error)
        return {"recorded": True}

    def reports(self):
        return [(c, a) for c, a in self.calls if c in work.REPORTS]


class WorkflowTests(unittest.TestCase):
    def test_refresh_delivery_claim_and_link_use_new_event(self):
        current = context()
        current["current_event_id"] = current["event"]["event_id"] = "event-2"
        current["current_version"] = "version-2"
        runner = FakeRunner(current)
        saved = work.capture(runner, PROJECT, "event-1", "task-1", "reviewer")
        self.assertEqual([c for c, _ in runner.calls], ["work-context", "verify-claim", "refresh-work-context",
                         "deliver", "work-context", "verify-claim", "link-task"])
        self.assertIn("event-2", runner.calls[-1][1])
        self.assertIn("version-2", work.flags(saved))
        self.assertIn("batch-1", work.flags(saved))

    def test_contribution_uses_current_target_not_tested_base(self):
        runner = FakeRunner()
        work.execute(runner, session(), "record-contribution", ["--outcome", "investigated"])
        args = runner.reports()[0][1]
        self.assertEqual(args[args.index("--base") + 1], BASE)
        self.assertEqual(args[args.index("--head") + 1], HEAD)
        with self.assertRaisesRegex(work.WorkflowError, "differs"):
            work.execute(runner, session(), "record-contribution", ["--base", OLD_BASE])
        self.assertEqual(len(runner.reports()), 1)

    def test_wake_only_retry_reuses_report_and_summary(self):
        current = context()
        wake_event(current)
        current["current_version"] = "version-2"
        current["snapshot"]["objects"]["fork"]["wake"] = 1
        current["fork"].update(wake=1, revision=2, next_attempt=123, retries=1)
        current["collaboration"]["revision"] = 2
        current["collaboration"]["items"][0]["link"] = {"version": "version-2"}
        runner = FakeRunner(current, failures=1)
        saved = []
        work.execute(runner, session(), "record-contribution", ["--report", "report-1", "--summary-file", "some file"], True, saved.append)
        self.assertEqual(len(runner.reports()), 2)
        for _, args in runner.reports():
            self.assertIn("report-1", args)
            self.assertIn("some file", args)
        self.assertIn("event-2", runner.reports()[1][1])
        self.assertEqual(len(saved), 1)

    def test_request_and_unknown_event_semantics_stop_retry(self):
        for payload in ({"kind": "work.requested", "reason": "Investigate a new failure", "changes": []},
                        {"kind": "new.directive", "changes": []}, None):
            current = context()
            wake_event(current)
            current["event"]["payload"] = payload
            runner = FakeRunner(current, failures=1)
            with self.assertRaisesRegex(work.WorkflowError, "material context changed"):
                work.execute(runner, session(), "record-work-result", [], True)
            self.assertEqual(len(runner.reports()), 1)
            self.assertNotIn("link-task", [c for c, _ in runner.calls])
        current = context()
        wake_event(current)
        current["event"]["payload"]["new_instructions"] = "Stop and investigate"
        self.assertFalse(work.bookkeeping_event(context(), current))

    def test_collaboration_bookkeeping_and_changed_delta(self):
        current = context()
        current["event"].update(event_id="event-2", payload={"kind": "collaboration.updated", "changes": [],
                                                         "collaboration_id": "batch-1", "origin_task": "task-1"})
        runner = FakeRunner(current, failures=1)
        work.execute(runner, session(), "record-work-result", [], True)
        self.assertEqual(len(runner.reports()), 2)
        wake_event(current)
        current["event"]["payload"]["changes"][0]["before"]["target_sha"] = "d" * 40
        self.assertFalse(work.bookkeeping_event(context(), current))

    def test_remote_head_takes_precedence_over_an_old_proof(self):
        value = session()
        value["context"]["snapshot"]["objects"]["fork"]["remote_head"] = "d" * 40
        args = work.operation_args("record-contribution", [], value)
        self.assertEqual(args[args.index("--head") + 1], "d" * 40)
        value["project"]["mode"] = "adapted-batches"
        value["context"]["snapshot"]["objects"]["fork"]["remote_head"] = ""
        value["context"]["fork"].pop("validated_head")
        with self.assertRaisesRegex(work.WorkflowError, "no complete head"):
            work.operation_args("record-contribution", [], value)

    def test_meaningful_changes_do_not_rebind_or_retry(self):
        changes = [
            lambda c: c["snapshot"]["objects"]["comment:1"].update(body="blocking finding"),
            lambda c: c["snapshot"]["objects"].update(check={"conclusion": "failure"}),
            lambda c: c["snapshot"]["objects"]["fork"].update(target_sha="d" * 40),
            lambda c: c["fork"].update(validated_head="d" * 40),
            lambda c: c["fork"].update(round="round-2"),
            lambda c: c["fork"]["validation"].update(id="proof-2"),
            lambda c: c["guide"].update(version="guide-2"),
            lambda c: c["collaboration"]["items"][0].update(revoked=True),
            lambda c: c["collaboration"]["items"][0].update(result={"outcome": "changes-requested"}),
            lambda c: c["collaboration"]["items"][0].update(executors=["writer"]),
            lambda c: c["fork"].update(queue_stale="target changed"),
        ]
        for change in changes:
            with self.subTest(change=change):
                current = context()
                change(current)
                runner = FakeRunner(current, failures=1)
                with self.assertRaisesRegex(work.WorkflowError, "material context changed"):
                    work.execute(runner, session(), "record-work-result", [], True)
                self.assertEqual(len(runner.reports()), 1)
                self.assertNotIn("link-task", [c for c, _ in runner.calls])

    def test_only_one_retry_and_no_ambiguous_failure_retry(self):
        for failures, error, count in ((2, work.STALE, 2), (1, "timeout after request", 1)):
            runner = FakeRunner(failures=failures)
            runner.error = error
            with self.assertRaises(work.CommandError):
                work.execute(runner, session(), "record-work-result", [], True)
            self.assertEqual(len(runner.reports()), count)

    def test_work_and_writes_cannot_opt_into_retry(self):
        for command in work.COMMANDS - work.REPORTS:
            runner = FakeRunner()
            with self.assertRaisesRegex(work.WorkflowError, "only report"):
                work.execute(runner, session(), command, [], True)
            self.assertEqual(runner.calls, [])

    def test_common_flag_overrides_are_rejected_before_execution(self):
        for arg in ("--task=other", "-executor=writer", "--event", "-version", "--batch=other"):
            runner = FakeRunner()
            with self.assertRaises(work.WorkflowError):
                work.execute(runner, session(), "test-worktree", [arg])
            self.assertEqual(runner.calls, [])

    def test_wrong_project_and_undelivered_event_never_link(self):
        for change in (lambda c: c["snapshot"]["objects"]["fork"].update(target_repo="other/repo"),
                       lambda c: c["event"].update(state="pending"), lambda c: c.update(stale=True)):
            current = context()
            change(current)
            runner = FakeRunner(current)
            with self.assertRaises(work.WorkflowError):
                work.capture(runner, PROJECT, "event-1", "task-1", "reviewer")
            self.assertNotIn("link-task", [c for c, _ in runner.calls])

    def test_lost_ownership_does_not_run_operation(self):
        runner = FakeRunner()
        runner.claim = False
        with self.assertRaises(work.WorkflowError):
            work.execute(runner, session(), "record-contribution", [], True)
        self.assertEqual(runner.reports(), [])

    def test_real_child_execute_false_and_literal_arguments(self):
        with tempfile.TemporaryDirectory() as tmp:
            child = Path(tmp) / "fake runner"
            child.write_text("#!/usr/bin/env python3\nimport json,sys\n"
                             "print(json.dumps({'execute':False} if sys.argv[1]=='verify-claim' else sys.argv[2:]))\n")
            child.chmod(0o700)
            runner = work.Runner(str(child))
            with self.assertRaisesRegex(work.WorkflowError, "no longer executable"):
                runner("verify-claim", [])
            literal = ["some file", "$(touch sentinel)", "`false`", "line\nbreak"]
            self.assertEqual(runner("echo", literal), literal)

    def test_session_private_atomic_and_no_symlink_clobber(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "context.json"
            work.save_session(path, session())
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(work.read_json(path), session())
            link = Path(tmp) / "link"
            link.symlink_to(path)
            with self.assertRaises(work.WorkflowError):
                work.save_session(link, {})
            self.assertEqual(work.read_json(path), session())

    def test_install_is_self_contained_and_preserves_existing_skill(self):
        with tempfile.TemporaryDirectory() as tmp:
            destination = Path(tmp) / ".agents/skills/upstream-sync"
            args = [sys.executable, str(SCRIPT), "install", "--destination", str(destination)]
            for key, value in PROJECT.items():
                args += ["--" + key.replace("_", "-"), value]
            result = subprocess.run(args, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(work.read_json(destination / "project.json"), PROJECT)
            installed = destination / "scripts/sync_work.py"
            self.assertEqual(installed.read_bytes(), SCRIPT.read_bytes())
            result = subprocess.run([sys.executable, str(installed), "--help"], capture_output=True)
            self.assertEqual(result.returncode, 0)
            self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)


if __name__ == "__main__":
    unittest.main()
