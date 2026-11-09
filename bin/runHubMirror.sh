#!/usr/bin/env bash
set -euo pipefail

# Runs a Hub Mirror: a Hub Proxy that eagerly keeps a complete copy of the
# upstream instead of only caching what's requested.
#
# Defaults (--addr :8082, --root data/mirror-repo, --upstream
# http://localhost:8080) are chosen so runHub.sh, runHubProxy.sh, and
# runHubMirror.sh can all run at once on one machine without colliding on
# port or repo root -- pointing at a runHub.sh started on this same
# machine. On a dedicated machine, override --addr/--root/--upstream freely.
#
# Any extra arguments are passed straight through to the server, e.g.:
#   ./runHubMirror.sh --mirror-concurrency 16

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ADDR=":8082"
ROOT="data/mirror-repo"
UPSTREAM="http://localhost:8080"
UPSTREAM_USER=""
UPSTREAM_PASS=""
EXTRA_ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --addr) ADDR="$2"; shift 2 ;;
    --root) ROOT="$2"; shift 2 ;;
    --upstream) UPSTREAM="$2"; shift 2 ;;
    --upstream-user) UPSTREAM_USER="$2"; shift 2 ;;
    --upstream-pass) UPSTREAM_PASS="$2"; shift 2 ;;
    *) EXTRA_ARGS+=("$1"); shift ;;
  esac
done

ARGS=(-addr "$ADDR" -root "$ROOT" -upstream "$UPSTREAM" -mirror)
[ -n "$UPSTREAM_USER" ] && ARGS+=(-upstream-user "$UPSTREAM_USER")
[ -n "$UPSTREAM_PASS" ] && ARGS+=(-upstream-pass "$UPSTREAM_PASS")
ARGS+=("${EXTRA_ARGS[@]}")

cd "$REPO_DIR"
exec go run . "${ARGS[@]}"
