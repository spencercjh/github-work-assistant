#!/usr/bin/env bash
set -euo pipefail
umask 077
cd -- "$(dirname -- "$0")/.."
for command in gh; do
  command -v "$command" >/dev/null || { printf 'Missing prerequisite: %s\n' "$command" >&2; exit 1; }
done
[[ -t 0 ]] || { printf 'Interactive setup needs a terminal. For automation use bin/ghraft init --help.\n' >&2; exit 1; }
[[ ! -e state/assistant ]] || { printf 'state/assistant already exists; use its run script. Setup never overwrites it.\n' >&2; exit 1; }
printf 'Storage: 1) local SQLite file  2) new MySQL container  3) existing database\n'
read -r -p 'Storage choice [1]: ' storage_choice
database_args=()
case "${storage_choice:-1}" in
  1)
    read -r -p 'SQLite path (empty for private instance directory): ' sqlite_path
    database_args=(--database sqlite)
    [[ -z "$sqlite_path" ]] || database_args+=(--sqlite-path "$sqlite_path")
    ;;
  2)
    command -v docker >/dev/null || { printf 'Docker is required for this option.\n' >&2; exit 1; }
    docker compose version >/dev/null 2>&1 || { printf 'Install the Docker Compose plugin first.\n' >&2; exit 1; }
    docker info >/dev/null 2>&1 || { printf 'Docker is unavailable to this user.\n' >&2; exit 1; }
    read -r -p 'Unused local database port [13306]: ' database_port
    database_args=(--database mysql --db-port "${database_port:-13306}")
    ;;
  3)
    read -r -p 'Database driver (mysql, seekdb, tidb, postgres): ' database_driver
    case "$database_driver" in mysql|seekdb|tidb|postgres|postgresql) ;; *) printf 'Unsupported driver.\n' >&2; exit 1 ;; esac
    read -r -p 'Private DSN file path (see docs/STORAGE.md): ' database_dsn_file
    database_args=(--database "$database_driver" --dsn-file "$database_dsn_file")
    ;;
  *) printf 'Choose 1, 2 or 3.\n' >&2; exit 1 ;;
esac
read -r -p 'Execution agent handle (on this computer, e.g. @reviewer): ' recipient
read -r -p 'Human handle for useful findings (optional): ' notify
read -r -s -p 'External Agent login token (hidden): ' credential
printf '\n'
token_file=$(mktemp)
trap 'rm -f -- "$token_file"' EXIT
printf '%s\n' "$credential" > "$token_file"
unset credential
bin/ghraft init --raft-token-file "$token_file" --recipient "$recipient" --notify "$notify" "${database_args[@]}"
rm -f -- "$token_file"
if [[ -f state/assistant/database.yml ]]; then
  docker compose -f state/assistant/database.yml up -d --wait
fi
state/assistant/run migrate
state/assistant/run doctor
printf '\nReady. Start the collector: state/assistant/run serve\nBackground service and reboot setup: docs/QUICKSTART.md\n'
