#!/usr/bin/env python3
"""Exercise a fresh installation using disposable storage and local API fixtures.

No real GitHub or Raft credential is read. Database images are only used for the optional managed MySQL check.
The database volume and private temporary directory are deleted on exit.
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import stat
import subprocess
import tempfile
import threading
import time
from urllib.parse import parse_qsl, urlsplit

REPO = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", choices=["sqlite","mysql","postgres"], default="sqlite")
    parser.add_argument("--delivery", choices=["shared", "dm"], default="shared")
    parser.add_argument("--dsn-file", help="Private disposable server DSN for external-database setup")
    parser.add_argument("--compose-binary", help="Optional standalone Docker Compose executable")
    args = parser.parse_args()
    if args.dsn_file:
        path = Path(args.dsn_file)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise SystemExit("Smoke-test DSN must be a private regular file")
        dsn = path.read_text().strip()
        safe = False
        if args.database == "postgres":
            try:
                parsed = urlsplit(dsn)
                safe = (parsed.scheme in ("postgres", "postgresql")
                        and parsed.hostname == "127.0.0.1"
                        and re.fullmatch(r"/[A-Za-z0-9_]+_test", parsed.path) is not None
                        and not parsed.fragment
                        and all(k in ("sslmode", "connect_timeout") for k, _ in parse_qsl(parsed.query)))
            except ValueError:
                pass
        elif args.database == "mysql":
            safe = re.fullmatch(r"[^\r\n]*@tcp\(127\.0\.0\.1:[0-9]+\)/[A-Za-z0-9_]+_test(?:\?[^\r\n]*)?", dsn) is not None
        if not safe:
            raise SystemExit("Refusing smoke test: requires a loopback DSN and a dedicated database ending in _test")
    compose = [args.compose_binary] if args.compose_binary else ["docker", "compose"]
    managed = args.database == "mysql" and not args.dsn_file
    for tool in ["go", "openssl"] + (["docker"] if managed else []):
        if not shutil.which(tool):
            raise SystemExit(f"Missing prerequisite: {tool}")
    if managed:
        subprocess.run(compose + ["version"], check=True, stdout=subprocess.DEVNULL)
    subprocess.run(["make", "build"], cwd=REPO, check=True)
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="ghraft-smoke-") as tmp:
        root = Path(tmp)
        instance = root / "assistant"
        (root / "bin").mkdir()
        token = "sk_agent_local_protocol_fixture"
        (root / "token").write_text(token + "\n")
        # These are synthetic test identities, not credentials for an actual server.
        identity = {"agent": {"id": "fixture-sender", "name": "collector", "external": True},
                    "server": {"id": "fixture-server", "slug": "fixture"},
                    "credential": {"capabilities": ["read", "send", "tasks", "channels"]}}
        received = []
        task_owner = None
        task_status = "todo"

        class API(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def reply(self, code, data):
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(data).encode())

            def do_GET(self):
                if self.headers.get("Authorization") != "Bearer " + token:
                    return self.reply(401, {})
                path = urlsplit(self.path).path
                if path == "/internal/agent-api/context":
                    return self.reply(200, identity)
                if path == "/internal/agent-api/channel-members":
                    return self.reply(200, {"channel": {"ref": "#github-work", "type": "channel"},
                                            "agents": [{"name": "collector"}, {"name": "executor"}],
                                            "humans": [{"name": "owner"}]})
                if path == "/internal/agent-api/tasks":
                    status_file = root / "task-status"
                    return self.reply(200, {"tasks": [{"taskNumber": 17, "messageId": "11111111-1111-1111-1111-111111111111",
                                                      "claimedByName": task_owner, "status": status_file.read_text() if status_file.exists() else task_status}]})
                if path == "/internal/agent-api/history":
                    query = dict(parse_qsl(urlsplit(self.path).query))
                    assert query["channel"] == "#github-work:11111111" and query["consume"] == "false"
                    assert query["around"] == "33333333-3333-4333-8333-333333333333"
                    report = root / "decision-report"
                    messages = []
                    if report.exists():
                        messages = [{"id": query["around"], "sender_name": "executor", "sender_type": "agent", "content": report.read_text()}]
                    return self.reply(200, {"target": query["channel"], "messages": messages})
                self.reply(404, {})

            def do_POST(self):
                if self.headers.get("Authorization") != "Bearer " + token:
                    return self.reply(401, {})
                if self.path not in ("/internal/agent-api/send", "/internal/agent-api/tasks"):
                    return self.reply(404, {})
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if args.delivery == "shared":
                    if self.path == "/internal/agent-api/send":
                        assert body["target"] == "#github-work:11111111"
                        assert body["idempotencyKey"].startswith("github-update:")
                        assert "@" + task_owner in body["content"]
                        if (root / "task-status").exists() and (root / "task-status").read_text() == "done" and (root / "closed").exists():
                            assert "原任务保持 Done" in body["content"]
                        assert body["continueAnyway"] is True
                        received.append(body)
                        return self.reply(200, {"ok": True, "state": "sent", "messageId": "22222222-2222-2222-2222-222222222222"})
                    assert self.path == "/internal/agent-api/tasks"
                    assert body["channel"] == "#github-work" and len(body["tasks"]) == 1 and "assignee" not in body
                    assert body["idempotencyKey"].startswith("github-task:")
                    title = body["tasks"][0]["title"]
                    description = body["tasks"][0]["description"]
                    assert "fixture external prose" in title and "＠fixture-human" in title
                    assert "@fixture-human" not in title + description
                    assert "fixture issue body" not in title + description
                    assert "首次读取当前状态" in description and "事件 ID" in description
                    received.append(body)
                    return self.reply(200, {"tasks": [{"taskNumber": 17, "messageId": "11111111-1111-1111-1111-111111111111"}]})
                assert body["target"] == "dm:@executor~agent"
                assert body["idempotencyKey"].startswith("github:")
                assert body["continueAnyway"] is True
                assert "fixture external prose" in body["content"] and "＠fixture-human" in body["content"]
                assert "@fixture-human" not in body["content"] and "fixture issue body" not in body["content"]
                received.append(body)
                self.reply(200, {"ok": True, "state": "sent", "messageId": "11111111-1111-1111-1111-111111111111"})

        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
                        "-keyout", str(root / "tls.key"), "-out", str(root / "tls.crt")],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), API)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(root / "tls.crt", root / "tls.key")
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        gh = root / "bin/gh"
        gh.write_text('''#!/usr/bin/env python3
import json,sys,datetime
from pathlib import Path
a=sys.argv[1:]
assert a[:5]==['api','--hostname','github.com','--method','GET'], 'non-GET rejected'
p=a[5]
if p=='user':
    if a[6:]==['--jq','.login']: print('fixture-user'); sys.exit(0)
    assert not a[6:]
    data={'login':'fixture-user'}
elif p.startswith('notifications?'):
    assert a[6:]==['--paginate','--slurp'] and 'per_page=50' in p
    data=[[{'updated_at':datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
            'subject':{'type':'Issue','url':'https://api.github.com/repos/example/fixture/issues/1'},
            'repository':{'private':False}}]]
elif p=='repos/example/fixture': data={'full_name':'example/fixture','private':False}
elif p=='repos/example/fixture/issues/1':
    data={'id':1,'number':1,'title':'fixture external prose @fixture-human','state':'open','body':'fixture issue body'}
    if (Path(__file__).resolve().parents[1]/'updated').exists(): data['title'] += ' updated'
    if (Path(__file__).resolve().parents[1]/'closed').exists(): data['state'] = 'closed'
    if (Path(__file__).resolve().parents[1]/'metadata').exists(): data['title'] += ' metadata'
elif p in ('repos/example/fixture/issues/1/comments?per_page=100','repos/example/fixture/issues/1/timeline?per_page=100'):
    assert a[6:]==['--paginate','--slurp']
    data=[[]]
else: sys.exit('unexpected fixture endpoint')
print(json.dumps(data))
''')
        gh.chmod(0o700)
        # Exercise the actual subprocess boundary without a live Raft write.
        # The fixture requires the caller's environment, not the sender token.
        raft = root / "bin/raft"
        raft.write_text('''#!/usr/bin/env python3
import os,sys
from pathlib import Path
root=Path(__file__).resolve().parents[1]
assert os.environ.get('RAFT_EXECUTOR_FIXTURE') == 'successor'
a=sys.argv[1:]
assert a[:7] == ['task','update','--target','#github-work','--number','17','--status']
assert len(a)==8 and a[7] in ('done','in_review','in_progress')
if (root/'unread-hold').exists(): sys.exit('Read the pending human message before retrying')
(root/'task-status').write_text(a[7])
with (root/'status-writes').open('a') as f: f.write(a[7]+'\\n')
print('Task status updated')
''')
        raft.chmod(0o700)
        env = {k: v for k, v in os.environ.items() if not k.startswith("GHRAFT_")}
        env.update(PATH=str(root / "bin") + os.pathsep + os.environ["PATH"], SSL_CERT_FILE=str(root / "tls.crt"))
        with socket.socket() as port:
            port.bind(("127.0.0.1", 0))
            db_port = port.getsockname()[1]
        base = [str(REPO / "bin/ghraft"), "init", "--dir", str(instance), "--raft-token-file", str(root / "token"),
                "--decision-maker", "@owner", "--db-port", str(db_port),
                "--raft-api", f"https://127.0.0.1:{server.server_port}"]
        base += ["--database", args.database]
        base += ["--task-channel", "#github-work"] if args.delivery == "shared" else ["--recipient", "@executor"]
        if args.dsn_file:
            base += ["--dsn-file", str(Path(args.dsn_file).resolve())]
        db = compose + ["-f", str(instance / "database.yml")]
        started = False
        proc = None

        def run(argv, fail=False, parse=False):
            result = subprocess.run(argv, env=env, cwd=REPO, text=True, capture_output=True, timeout=120)
            if (result.returncode != 0) != fail:
                raise RuntimeError(f"Unexpected status for {argv[0]} {argv[1:2]}: {result.stderr}")
            return json.loads(result.stdout) if parse else result.stdout

        try:
            run(base)
            key_before = (instance / "private/key").read_bytes()
            run(base, fail=True)
            assert (instance / "private/key").read_bytes() == key_before
            if managed:
                started = True
                subprocess.run(db + ["up", "-d", "--wait", "--wait-timeout", "180"], check=True, env=env)
            else:
                assert not (instance / "database.yml").exists()
            cli = [str(instance / "run")]
            if args.database == "sqlite":
                database_path = Path((instance / "private/dsn").read_text().strip())
                assert not database_path.exists()
                run(cli + ["doctor"], fail=True)
                assert not database_path.exists(), "doctor created a database"
            run(cli + ["migrate"])
            assert all(c["ok"] for c in run(cli + ["doctor"], parse=True))
            (instance / "private/raft-token").write_text("sk_agent_invalid_fixture\n")
            run(cli + ["doctor"], fail=True)
            assert not received, "doctor unexpectedly sent a message"
            (instance / "private/raft-token").write_text(token + "\n")
            # The generated entry point must not inherit another installation's secrets.
            env["GHRAFT_DSN_FILE"] = "/nonexistent/other-instance"
            run(cli + ["collect"])
            events = run(cli + ["events"], parse=True)
            assert len(events) == 1 and events[0]["state"] == "pending"
            event = events[0]["event_id"]
            context = run(cli + ["work-context", "--event", event], parse=True)
            assert context["guide"]["mode"] == "read-only"
            assert context["guide"]["decision_maker"] == "@owner"
            assert json.loads((instance / "config.json").read_text())["notify_target"] == ""
            run(cli + ["deliver"])
            assert len(received) == 1
            assert run(cli + ["events"], parse=True)[0]["state"] == "accepted"
            assert len(run(cli + ["pending"], parse=True)) == 1, "acceptance was confused with processing"
            run(cli + ["collect"])
            run(cli + ["deliver"])
            assert len(received) == 1, "restart duplicated an accepted event"
            (root / "reason").write_text("Synthetic fixture needs no user notification.\n")
            if args.delivery == "shared":
                verify = cli + ["verify-claim", "--event", event]
                run(verify + ["--executor", "@executor"], fail=True)  # Still unclaimed.
                task_owner = "executor"  # Model a successful native Raft claim.
                task_status = "in_progress"
                assert run(verify + ["--executor", "@executor"], parse=True)["execute"] is True
                assert run(verify + ["--executor", "@loser"], parse=True)["execute"] is False
                declined = run(cli + ["record-triage", "--event", event, "--executor", "@loser", "--outcome", "quiet", "--reason-file", str(root / "reason")], parse=True)
                assert declined["execute"] is False and declined["recorded"] is False
                # A local result is not a human request. Only an actual report
                # addressed in the original discussion may become needs-decision.
                work_args = ["--event", event, "--executor", "@executor", "--version", context["current_version"],
                             "--guide", context["guide"]["version"], "--task", "11111111-1111-1111-1111-111111111111"]
                run(cli + ["link-task"] + work_args)
                decision_args = cli + ["record-work-result"] + work_args + ["--report", "33333333-3333-4333-8333-333333333333",
                                 "--outcome", "needs-decision", "--summary-file", str(root / "reason")]
                run(decision_args, fail=True)
                (root / "decision-report").write_text("`@owner` Choose the migration policy.")
                run(decision_args, fail=True)
                (root / "decision-report").write_text("@owner Should we keep the old API? I recommend keeping it during migration.")
                run(decision_args)
                run(decision_args)  # Reusing the same request never sends another mention.
                assert len(received) == 1
            run(cli + ["record-triage", "--event", event, "--executor", "@executor", "--outcome", "quiet", "--reason-file", str(root / "reason")])
            assert not run(cli + ["pending"], parse=True)

            if args.delivery == "shared":
                task_status = "done"
                (root / "updated").touch()
                run(cli + ["collect"])
                newer = next(e for e in run(cli + ["events"], parse=True) if e["event_id"] != event)
                run(cli + ["deliver"])
                assert len(received) == 2 and "target" in received[1], "new event forked a task"
                context = run(cli + ["work-context", "--event", newer["event_id"]], parse=True)
                assert context["discussion"]["messageId"] == "11111111-1111-1111-1111-111111111111"
                assert context["event"]["receipt"]["notificationMessageId"] == "22222222-2222-2222-2222-222222222222"
                assert task_owner == "executor" and task_status == "done", "collector changed task ownership/state"
                newer_verify = cli + ["verify-claim", "--event", newer["event_id"]]
                assert run(newer_verify + ["--executor", "@executor"], parse=True)["execute"] is False
                task_status = "in_progress"  # The same owner reads the thread and resumes.
                assert run(newer_verify + ["--executor", "@executor"], parse=True)["execute"] is True
                run(cli + ["record-triage", "--event", newer["event_id"], "--executor", "@executor", "--outcome", "quiet", "--reason-file", str(root / "reason")])
                task_owner = "successor"  # An explicit manual handoff of the same task.
                task_status = "todo"  # Assignment reserves ownership without starting work.
                assert run(newer_verify + ["--executor", "@executor"], parse=True)["execute"] is False
                assert run(newer_verify + ["--executor", "@successor"], parse=True)["execute"] is False
                task_status = "in_progress"  # The assigned successor claims the original task.
                assert run(newer_verify + ["--executor", "@successor"], parse=True)["execute"] is True
                run(cli + ["deliver"])
                assert len(received) == 2 and not run(cli + ["pending"], parse=True)

                # A closed Issue finishes the existing task through the owner's
                # CLI; an unread hold must not be treated as completed.
                env["RAFT_EXECUTOR_FIXTURE"] = "successor"
                (root / "closed").touch()
                run(cli + ["collect"])
                closed_event = run(cli + ["work-context", "--event", event], parse=True)["current_event_id"]
                run(cli + ["deliver"])
                context = run(cli + ["work-context", "--event", closed_event], parse=True)
                def finish_args(context):
                    return cli + ["finish-work", "--executor", "@successor", "--event", context["current_event_id"],
                                  "--version", context["current_version"], "--guide", context["guide"]["version"],
                                  "--task", context["discussion"]["messageId"]]
                run(finish_args(context), fail=True)  # Delivery alone is insufficient.
                assert not (root / "task-status").exists()
                run(cli + ["record-triage", "--event", closed_event, "--executor", "@successor", "--outcome", "quiet", "--reason-file", str(root / "reason")])
                (root / "unread-hold").touch()
                run(finish_args(context), fail=True)
                assert not (root / "task-status").exists()
                (root / "unread-hold").unlink()
                assert run(finish_args(context), parse=True)["status"] == "done"
                assert run(finish_args(context), parse=True)["status"] == "done"
                assert (root / "status-writes").read_text().splitlines() == ["done"]

                # Post-close metadata stays Done; reopening reuses that task.
                (root / "metadata").touch()
                run(cli + ["collect"])
                run(cli + ["deliver"])
                context = run(cli + ["work-context", "--event", event], parse=True)
                run(cli + ["record-triage", "--event", context["current_event_id"], "--executor", "@successor", "--outcome", "quiet", "--reason-file", str(root / "reason")])
                assert (root / "task-status").read_text() == "done"
                assert len(received) == 4 and all("target" in body for body in received[1:])
                (root / "closed").unlink()
                run(cli + ["collect"])
                run(cli + ["deliver"])
                assert "恢复为 in_progress" in received[-1]["content"]
                (root / "task-status").write_text("in_progress")  # Owner reads and resumes.
                context = run(cli + ["work-context", "--event", event], parse=True)
                run(cli + ["record-triage", "--event", context["current_event_id"], "--executor", "@successor", "--outcome", "quiet", "--reason-file", str(root / "reason")])
                assert run(finish_args(context), parse=True)["status"] == "in_review"
                assert not run(cli + ["pending"], parse=True)

            preview = run(cli + ["cleanup", "--dry-run"], parse=True)
            assert preview["dry_run"] and preview["history_retention_days"] == 30
            assert preview["event_bodies"] == 0, "newly completed event expired immediately"
            cleaned = run(cli + ["cleanup"], parse=True)
            assert not cleaned["dry_run"] and cleaned["event_bodies"] == 0
            assert not run(cli + ["pending"], parse=True)
            status = run(cli + ["status"], parse=True)
            assert status["watches"][0]["last_success"] > 0 and status["pending_delivery"] == 0
            proc = subprocess.Popen(cli + ["serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            time.sleep(1)
            assert proc.poll() is None, "service exited during startup"
            run(cli + ["serve"], fail=True)
            proc.terminate()
            assert proc.wait(timeout=15) == 0, "service did not stop gracefully"
            print(f"PASS ({args.database}, {args.delivery}): private init, no overwrite, migration, diagnostics/auth failure, discovery, delivery, claim verification, durable processing, restart and service lock; local API fixtures only.")
        finally:
            if proc is not None and proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=15)
            if started:
                subprocess.run(db + ["down", "--volumes", "--remove-orphans"], env=env, check=True)
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
