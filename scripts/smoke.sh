#!/usr/bin/env bash
# Builds kanshi, starts it against a scratch storage root, and checks that the
# dashboard, the vitals and the storage walk all answer. Runs the same way on
# Linux and on Windows (Git Bash), which is the point: CI is the only place the
# Windows build actually gets to run.
set -euo pipefail

port=${KANSHI_SMOKE_PORT:-8177}
bin=./kanshi-smoke
[ "${RUNNER_OS:-}" = Windows ] || [ "${OS:-}" = Windows_NT ] && bin=./kanshi-smoke.exe
tmp=${RUNNER_TEMP:-$(mktemp -d)}
root="$tmp/kanshi-smoke-root"

go build -o "$bin" .
mkdir -p "$root/folder"
head -c 1048576 /dev/urandom > "$root/folder/blob.bin"

KANSHI_PORT=$port KANSHI_STORAGE_ROOTS="$root" KANSHI_CONFIG="$tmp/kanshi-smoke.env" \
  "$bin" -open=false > smoke.log 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true; echo "--- kanshi output"; cat smoke.log' EXIT

url=http://127.0.0.1:$port
for _ in $(seq 1 30); do
  curl -fsS "$url/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$url/healthz"; echo

echo "--- vitals"
curl -fsS "$url/api/vitals" | tee vitals.json; echo
grep -q '"count":[1-9]' vitals.json          # found CPU cores
grep -q '"memory":{"total":[1-9]' vitals.json # and memory

echo "--- storage"
for _ in $(seq 1 30); do
  curl -fsS "$url/api/storage" > storage.json
  grep -q '"size":[1-9]' storage.json && break
  sleep 1
done
cat storage.json; echo
grep -q '"size":[1-9]' storage.json
curl -fsS "$url/api/storage/dir?root=0&path=folder" | grep -q '"file_count":1'

echo "--- page"
curl -fsS "$url/" | grep -q 'app.js?v='
curl -fsS "$url/api/access" | grep -q '"mode":"local"'
echo "smoke test passed"
