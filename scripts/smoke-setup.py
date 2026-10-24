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
                    "credential": {"capabilities": ["read", "send"]}}
        received = []

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
                if self.path != "/internal/agent-api/context":
                    return self.reply(404, {})
                self.reply(200, identity)

            def do_POST(self):
                if self.headers.get("Authorization") != "Bearer " + token:
                    return self.reply(401, {})
                if self.path != "/internal/agent-api/send":
                    return self.reply(404, {})
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                assert body["target"] == "dm:@executor~agent"
                assert body["idempotencyKey"].startswith("github:")
                assert body["continueAnyway"] is True
                assert "fixture external prose" not in body["content"]
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
    data={'id':1,'number':1,'title':'fixture external prose','state':'open','body':'fixture external prose'}
elif p in ('repos/example/fixture/issues/1/comments?per_page=100','repos/example/fixture/issues/1/timeline?per_page=100'):
    assert a[6:]==['--paginate','--slurp']
    data=[[]]
else: sys.exit('unexpected fixture endpoint')
print(json.dumps(data))
''')
        gh.chmod(0o700)
        env = {k: v for k, v in os.environ.items() if not k.startswith("GHRAFT_")}
        env.update(PATH=str(root / "bin") + os.pathsep + os.environ["PATH"], SSL_CERT_FILE=str(root / "tls.crt"))
        with socket.socket() as port:
            port.bind(("127.0.0.1", 0))
            db_port = port.getsockname()[1]
        base = [str(REPO / "bin/ghraft"), "init", "--dir", str(instance), "--raft-token-file", str(root / "token"),
                "--recipient", "@executor", "--notify", "@owner", "--db-port", str(db_port),
                "--raft-api", f"https://127.0.0.1:{server.server_port}"]
        base += ["--database", args.database]
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
            run(cli + ["deliver"])
            assert len(received) == 1
            assert run(cli + ["events"], parse=True)[0]["state"] == "accepted"
            assert len(run(cli + ["pending"], parse=True)) == 1, "acceptance was confused with processing"
            run(cli + ["collect"])
            run(cli + ["deliver"])
            assert len(received) == 1, "restart duplicated an accepted event"
            (root / "reason").write_text("Synthetic fixture needs no user notification.\n")
            run(cli + ["record-triage", "--event", event, "--outcome", "quiet", "--reason-file", str(root / "reason")])
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
            print(f"PASS ({args.database}): private init, no overwrite, migration, diagnostics/auth failure, discovery, delivery, durable processing, restart and service lock; local API fixtures only.")
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
