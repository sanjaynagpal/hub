#!/usr/bin/env bash
set -euo pipefail

# Runs a standalone Hub (Maven repo server).
#
# Defaults (--addr :8080, --root data/hub-repo, proxies allowed) are chosen
# so runHub.sh, runHubProxy.sh, and runHubMirror.sh can all run at once on
# one machine without colliding on port or repo root. On a dedicated
# machine, override --addr/--root freely -- there's nothing else on that
# host to collide with.
#
# Any extra arguments are passed straight through to the server, e.g.:
#   ./runHub.sh --read-auth -max-upload-size 0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ADDR=":8080"
ROOT="data/hub-repo"
HUB_USER=""
HUB_PASS=""
ALLOW_PROXIES=1
READ_ONLY=0
EXTRA_ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --addr) ADDR="$2"; shift 2 ;;
    --root) ROOT="$2"; shift 2 ;;
    --user) HUB_USER="$2"; shift 2 ;;
    --pass) HUB_PASS="$2"; shift 2 ;;
    --read-only) READ_ONLY=1; shift ;;
    --no-allow-proxies) ALLOW_PROXIES=0; shift ;;
    *) EXTRA_ARGS+=("$1"); shift ;;
  esac
done

ARGS=(-addr "$ADDR" -root "$ROOT")
[ "$ALLOW_PROXIES" = "1" ] && ARGS+=(-allow-proxies)
[ "$READ_ONLY" = "1" ] && ARGS+=(-read-only)
[ -n "$HUB_USER" ] && ARGS+=(-user "$HUB_USER")
[ -n "$HUB_PASS" ] && ARGS+=(-pass "$HUB_PASS")
ARGS+=("${EXTRA_ARGS[@]}")

cd "$REPO_DIR"
exec go run . "${ARGS[@]}"
