#!/usr/bin/env bash
# Run the Go API (:8080) and the Vite dev server (:5173) together, and make
# sure BOTH die when this script is interrupted — the whole point is that you
# can never end up with one half running and the other dead (which shows up as
# "localhost won't load" or a 502 from the proxy).
set -euo pipefail
cd "$(dirname "$0")/.."

export SANDBOX_TOKEN="${SANDBOX_TOKEN:-devtoken}"

# Fail early with a clear message if Docker isn't up — otherwise the first Run
# just returns an opaque error.
if ! docker info >/dev/null 2>&1; then
  echo "✗ Docker daemon not reachable. Start Docker Desktop (or Colima) first." >&2
  exit 1
fi

# Refuse to start if 8080 is already taken, rather than silently colliding with
# a stale server (a mistake we have hit repeatedly).
if lsof -ti:8080 >/dev/null 2>&1; then
  echo "✗ Port 8080 is already in use. Stop the other server first:" >&2
  echo "    kill \$(lsof -ti:8080)" >&2
  exit 1
fi

pids=()
devbin=""
cleanup() {
  echo
  echo "shutting down dev servers..."
  # SIGTERM the API so its own handler destroys any live sessions.
  for pid in "${pids[@]}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  [ -n "$devbin" ] && rm -f "$devbin"
}
# Any exit path — Ctrl-C, a child dying, an error — runs cleanup once.
trap cleanup EXIT INT TERM

# Build and run the binary directly rather than `go run`: `go run` execs the
# compiled server as a CHILD, so killing `go run` orphans the real process (it
# reparents to init and keeps holding :8080). Running the binary ourselves means
# the PID we track IS the server, so SIGTERM reaches its graceful-shutdown
# handler and the port is actually freed.
echo "→ building API..."
devbin="$(mktemp -t sandbox-dev-server)"
go build -o "$devbin" ./cmd/server

echo "→ API   http://localhost:8080  ($devbin)"
"$devbin" &
pids+=($!)

echo "→ UI    http://localhost:5173  (vite, proxies to :8080)"
npm --prefix web run dev &
pids+=($!)

echo
echo "both running — open http://localhost:5173 · Ctrl-C stops both"

# If EITHER process exits, tear the other down too, so we never leave one half
# alive. `wait -n` would be cleaner but macOS ships bash 3.2, which lacks it —
# so poll instead. The EXIT trap does the actual teardown.
while :; do
  for pid in "${pids[@]}"; do
    kill -0 "$pid" 2>/dev/null || exit 0
  done
  sleep 1
done
