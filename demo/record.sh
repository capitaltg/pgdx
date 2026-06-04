#!/usr/bin/env bash
# Render demo/shell.gif: build a static pgdx, spin up a throwaway demo Postgres,
# and run the VHS tape against it — all via Docker, nothing installed on the host.
#
# Usage: demo/record.sh            (run from the repo root or anywhere)
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$DEMO_DIR/.." && pwd)"
NET=pgdx-demo-net
PG=pgdx-demo-pg
ARCH="$(uname -m)"; [ "$ARCH" = aarch64 ] && GOARCH=arm64 || GOARCH=amd64

cleanup() { docker rm -f "$PG" >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "==> building static pgdx (linux/$GOARCH)"
( cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$DEMO_DIR/pgdx" . )

echo "==> starting demo Postgres"
docker network create "$NET" >/dev/null
docker run -d --name "$PG" --network "$NET" \
  -e POSTGRES_USER=demo -e POSTGRES_PASSWORD=demo -e POSTGRES_DB=shop \
  postgres:17-alpine >/dev/null

echo "==> waiting for Postgres to accept connections"
# The official image runs a temporary server for initdb, then restarts. Poll a real
# query against the target DB and require two consecutive successes ~1s apart, so we
# don't latch onto the init server during the brief restart window.
ok=0
for i in $(seq 1 60); do
  if docker exec "$PG" psql -U demo -d shop -tAc 'select 1' >/dev/null 2>&1; then
    ok=$((ok + 1)); [ "$ok" -ge 2 ] && break
  else
    ok=0
  fi
  sleep 1
done
[ "$ok" -ge 2 ] || { echo "Postgres never became ready"; docker logs "$PG" | tail; exit 1; }

echo "==> loading demo schema"
docker exec -i "$PG" psql -U demo -d shop -q -v ON_ERROR_STOP=1 < "$DEMO_DIR/schema.sql"

echo "==> rendering tape with VHS"
docker run --rm --network "$NET" -v "$DEMO_DIR:/vhs" -w /vhs \
  ghcr.io/charmbracelet/vhs shell.tape

echo "==> done: $DEMO_DIR/shell.gif"
