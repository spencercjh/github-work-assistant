#!/usr/bin/env bash
set -euo pipefail
umask 077
cd -- "$(dirname -- "$0")/.."
for command in gh docker; do
  command -v "$command" >/dev/null || { printf 'Missing prerequisite: %s\n' "$command" >&2; exit 1; }
done
docker compose version >/dev/null 2>&1 || { printf 'Install the Docker Compose plugin first. See docs/QUICKSTART.md.\n' >&2; exit 1; }
docker info >/dev/null 2>&1 || { printf 'Docker is unavailable to this user; start Docker and check access first.\n' >&2; exit 1; }
[[ -t 0 ]] || { printf 'Interactive setup needs a terminal. For automation use bin/ghraft init --help.\n' >&2; exit 1; }
[[ ! -e state/assistant ]] || { printf 'state/assistant already exists; use its run script. Setup never overwrites it.\n' >&2; exit 1; }
read -r -p 'Execution agent handle (on this computer, e.g. @reviewer): ' recipient
read -r -p 'Human handle for useful findings (optional): ' notify
read -r -s -p 'External Agent login token (hidden): ' credential
printf '\n'
token_file=$(mktemp)
trap 'rm -f -- "$token_file"' EXIT
printf '%s\n' "$credential" > "$token_file"
unset credential
bin/ghraft init --raft-token-file "$token_file" --recipient "$recipient" --notify "$notify"
rm -f -- "$token_file"
docker compose -f state/assistant/database.yml up -d --wait
state/assistant/run migrate
state/assistant/run doctor
printf '\nReady. Start the collector: state/assistant/run serve\nBackground service and reboot setup: docs/QUICKSTART.md\n'
