#!/usr/bin/env python3
"""Read-only export of public pilot state. Freeze old collector before final export.
Usage: python3 scripts/export-legacy.py OLD_PILOT_DIR > PRIVATE_EXPORT.json
Private watches/bodies/history are intentionally excluded. No tokens are read.
"""
import datetime as dt
import json
from pathlib import Path
import sqlite3
import sys

root = Path(sys.argv[1])
source = sqlite3.connect(f'file:{root}/state/assistant.db?mode=ro', uri=True)
source.row_factory = sqlite3.Row
ledger = sqlite3.connect(f'file:{root}/state/triage.db?mode=ro', uri=True)
ledger.row_factory = sqlite3.Row
processed = {r['marker']: dict(r) for r in ledger.execute('SELECT * FROM processed')}
meta = {r['key']: json.loads(r['value']) for r in source.execute('SELECT * FROM meta')}
public = list(source.execute('SELECT * FROM watches WHERE enabled=1 AND private=0 ORDER BY key'))
keys = {r['key'] for r in public}
batches = [dict(r) for r in source.execute('SELECT * FROM deliveries ORDER BY id') if r['watch'] in keys]
pending = [r['marker'] for r in batches if r['marker'] not in processed]
if pending:
    raise SystemExit('Finish public legacy triage before export: ' + ', '.join(pending))
watches = []
for w in public:
    objects = {r['key']: json.loads(r['body']) for r in source.execute('SELECT key,body FROM objects WHERE watch=?', (w['key'],))}
    # Old crossrefs discarded visibility metadata; don't carry unknown/private links forward.
    objects = {k: v for k, v in objects.items() if v.get('event') != 'cross-referenced'}
    watches.append(dict(repo=w['repo'],number=w['number'],private=False,objects=objects,
        observed_at=int(dt.datetime.fromisoformat(w['last_success'].replace('Z','+00:00')).timestamp())))
history = [dict(processed[r['marker']],watch=r['watch'],target=r['target']) for r in batches]
json.dump(dict(source='github-assistant-pilot-v1',login=meta['github_login'],cursor=meta['notifications_cursor'],
    pending=0,watches=watches,history=history),sys.stdout,ensure_ascii=False,sort_keys=True,separators=(',',':'))
