#!/usr/bin/env python3
"""Repository-local orchestration of the existing ghraft instance CLI.

No credentials, task claiming, source writes, or GitHub mutations are implemented
here. The configured instance remains the authority for every work operation.
"""

import argparse
import copy
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


STALE = "work evidence or repository guidance changed; refresh context"
COMMANDS = {
    "prepare-worktree", "verify-worktree", "test-worktree", "check-github-action",
    "start-sync-batch", "select-sync-batch", "rebuild-sync-batch",
    "register-work-item", "revoke-work-item", "record-work-result",
    "record-contribution", "finish-work",
}
REPORTS = {"record-work-result", "record-contribution"}
COMMON = {"event", "version", "guide", "task", "executor", "batch"}
SHA = re.compile(r"[0-9a-f]{40,64}\Z")


class WorkflowError(Exception):
    pass


class CommandError(WorkflowError):
    def __init__(self, command, detail):
        self.command = command
        self.detail = detail
        super().__init__(f"{command} failed: {detail}")


def read_json(path):
    with open(path, encoding="utf-8") as stream:
        return json.load(stream)


def save_session(path, session):
    path = Path(path)
    # Context may contain private review text. Keep it outside versioned files.
    if path.is_symlink():
        raise WorkflowError("context output must not be a symbolic link")
    fd, tmp = tempfile.mkstemp(prefix=".sync-context-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(session, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


class Runner:
    def __init__(self, executable):
        if not executable or not Path(executable).is_absolute():
            raise WorkflowError("provide the installed instance's absolute --runner path or GHRAFT_RUN")
        self.executable = executable

    def __call__(self, command, args):
        # Argument arrays preserve literal paths/text; never evaluate a shell.
        result = subprocess.run([self.executable, command, *args], text=True,
                                capture_output=True, check=False)
        if result.returncode:
            raise CommandError(command, result.stderr.strip() or "nonzero exit; inspect instance logs")
        if not result.stdout.strip():
            if command == "deliver":
                return None
            raise WorkflowError(f"{command} returned no JSON result")
        value = json.loads(result.stdout)
        if isinstance(value, dict) and value.get("execute") is False:
            raise WorkflowError("task is no longer executable by this identity; read its live ownership and status")
        return value


def check_project(context, project):
    facts = context["snapshot"]["objects"]["fork"]
    actual = {
        "fork": facts["id"], "target": facts["target_repo"],
        "upstream": facts["upstream_repo"], "upstream_branch": facts["upstream_branch"],
        # In adapted queues an individual PR can target its predecessor branch.
        "target_branch": facts.get("integration_branch", facts["target_branch"]),
        "mode": context["guide"].get("fork_sync_mode") or "upstream-only",
    }
    if actual != project or context["event"]["resource_key"] != "fork:" + project["fork"]:
        raise WorkflowError("context does not match this repository's project.json; inspect the selected instance/event")


def flags(session):
    context = session["context"]
    result = ["--event", context["event"]["event_id"], "--version", context["current_version"],
              "--guide", context["guide"]["version"], "--task", session["task"],
              "--executor", session["executor"]]
    batch = context["fork"].get("batch_id")
    if batch:
        result += ["--batch", batch]
    return result


def material(context):
    """Ignore only known bookkeeping; retain all feedback, proofs and scopes."""
    result = copy.deepcopy({k: context.get(k) for k in
                            ("snapshot", "fork", "guide", "collaboration", "processed_history")})
    result["snapshot"]["objects"]["fork"].pop("wake", None)

    def progress(value):
        for key in ("revision", "next_attempt", "retries", "wake"):
            value.pop(key, None)
        for batch in (value.get("queue") or {}).get("batches", []):
            progress(batch["progress"])

    progress(result["fork"])
    collaboration = result.get("collaboration")
    if collaboration:
        collaboration.pop("revision", None)
        for item in collaboration.get("items", []):
            # Linking does not grant authority. Live claim checks still run.
            item.pop("link", None)
    return result


def capture(runner, project, event, task, executor, batch="", previous=None):
    identity = ["--task", task, "--executor", executor]
    scope = ["--batch", batch] if batch else []
    initial = runner("work-context", ["--event", event, *identity, *scope])
    check_project(initial, project)
    runner("verify-claim", ["--event", event, *identity, *scope])
    refreshed = runner("refresh-work-context", ["--event", event, *identity, *scope])
    check_project(refreshed, project)
    runner("deliver", [])
    current = refreshed["current_event_id"]
    context = runner("work-context", ["--event", current, *identity, *scope])
    check_project(context, project)
    if context.get("stale") or context["event"]["state"] != "accepted":
        raise WorkflowError("current event is not fresh and delivered; inspect delivery status before capturing again")
    if previous is not None and material(previous) != material(context):
        raise WorkflowError("material context changed; read new feedback, source/target, proof and guidance before making a new report (old context retained)")
    runner("verify-claim", ["--event", current, *identity, *scope])
    session = {"schema": 1, "project": project, "task": task,
               "executor": executor, "context": context}
    runner("link-task", flags(session))
    return session


def operation_args(command, extra, session):
    extra = list(extra)
    # Go accepts both -flag and --flag. Block both spellings and = forms.
    for arg in extra:
        if arg.startswith("-") and arg.lstrip("-").split("=", 1)[0] in COMMON:
            raise WorkflowError("common work flags come from the captured context; capture a new context to change scope")
    if command == "record-contribution":
        context = session["context"]
        facts = context["snapshot"]["objects"]["fork"]
        head = facts.get("remote_head") or context["fork"].get("validated_head")
        if not head and session["project"]["mode"] == "upstream-only":
            head = facts["upstream_sha"]
        head = head or ""
        base = facts["target_sha"]
        if not SHA.fullmatch(head) or not SHA.fullmatch(base):
            raise WorkflowError("context has no complete head/current target SHA for a contribution")
        for name, value in (("head", head), ("base", base)):
            supplied = []
            for i, arg in enumerate(extra):
                key, _, inline = arg.lstrip("-").partition("=")
                if arg.startswith("-") and key == name:
                    supplied.append(inline if "=" in arg else extra[i + 1] if i + 1 < len(extra) else "")
            if supplied and any(v != value for v in supplied):
                raise WorkflowError(f"--{name} differs from the captured context; read the current version before reporting")
            if not supplied:
                extra += ["--" + name, value]
    return extra


def execute(runner, session, command, extra, retry_stale=False, save=None):
    if session.get("schema") != 1 or command not in COMMANDS:
        raise WorkflowError("unsupported context schema or work command")
    if retry_stale and command not in REPORTS:
        raise WorkflowError("only report registration supports --retry-stale; work and write checks run once")
    original = session
    extra = operation_args(command, extra, session)
    for attempt in range(2 if retry_stale else 1):
        try:
            runner("verify-claim", flags(session))
            return runner(command, [*flags(session), *extra])
        except CommandError as error:
            if attempt or not retry_stale or error.command != command or STALE not in error.detail:
                raise
            # Do not retry network/timeout/access/ownership or ambiguous failures.
            session = capture(runner, session["project"], session["context"]["event"]["event_id"],
                              session["task"], session["executor"],
                              session["context"]["fork"].get("batch_id", ""),
                              previous=original["context"])
            if save:
                save(session)
    raise WorkflowError("report still stale after one refresh; inspect concurrent activity")


def install(args):
    destination = Path(args.destination)
    if destination.exists():
        raise WorkflowError("destination exists; review changes before replacing an installed skill")
    root = Path(__file__).resolve().parents[1]
    project = {key: getattr(args, key) for key in
               ("fork", "target", "upstream", "target_branch", "upstream_branch", "mode")}
    if not re.fullmatch(r"[a-zA-Z0-9_-]+", project["fork"]):
        raise WorkflowError("invalid fork identifier")
    for key in ("target", "upstream"):
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", project[key]):
            raise WorkflowError("repository must be owner/name")
    destination.mkdir(parents=True)
    (destination / "scripts").mkdir()
    shutil.copyfile(root / "SKILL.md", destination / "SKILL.md")
    shutil.copyfile(__file__, destination / "scripts" / "sync_work.py")
    (destination / "project.json").write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")
    return {"installed": str(destination), "project": project}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    installer = commands.add_parser("install", help="copy the skill to a fork repository's new .agents/skills/upstream-sync directory")
    installer.add_argument("--destination", required=True)
    for name in ("fork", "target", "upstream", "target-branch", "upstream-branch"):
        installer.add_argument("--" + name, required=True)
    installer.add_argument("--mode", choices=("upstream-only", "adapted-batches"), required=True)
    context = commands.add_parser("context", help="refresh, deliver, verify and link a task; save context for review")
    operation = commands.add_parser("run", help="run a checked command using reviewed context")
    for child in (context, operation):
        child.add_argument("--runner", default=os.environ.get("GHRAFT_RUN"))
        child.add_argument("--context-file", required=True)
        child.add_argument("--project", default=str(Path(__file__).resolve().parents[1] / "project.json"))
    context.add_argument("--event", required=True)
    context.add_argument("--task", required=True)
    context.add_argument("--executor", required=True)
    context.add_argument("--batch", default="")
    operation.add_argument("--retry-stale", action="store_true")
    operation.add_argument("operation", choices=sorted(COMMANDS))
    operation.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    try:
        if args.command == "install":
            result = install(args)
        else:
            runner = Runner(args.runner)
            project = read_json(args.project)
            if args.command == "context":
                session = capture(runner, project, args.event, args.task, args.executor, args.batch)
                save_session(args.context_file, session)
                w = session["context"]
                result = {"context_file": args.context_file, "event": w["event"]["event_id"],
                          "batch": w["fork"].get("batch_id"), "pr": w["fork"].get("pr"),
                          "phase": w["fork"]["phase"], "instruction": "Read this context and task discussion before executing work."}
            else:
                session = read_json(args.context_file)
                if session["project"] != project:
                    raise WorkflowError("context file belongs to another repository")
                check_project(session["context"], project)
                extra = args.arguments[1:] if args.arguments[:1] == ["--"] else args.arguments
                result = execute(runner, session, args.operation, extra, args.retry_stale,
                                 save=lambda value: save_session(args.context_file, value))
        print(json.dumps(result, ensure_ascii=False))
    except (WorkflowError, OSError, ValueError, KeyError, TypeError) as error:
        print(f"sync-work: {error}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("sync-work: interrupted; inspect the original attempt before retrying", file=sys.stderr)
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
