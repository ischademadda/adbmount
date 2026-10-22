#!/usr/bin/env bash
# Real FUSE-T mount against the fake adb: up → read → write → down. Needs FUSE-T; not run in CI.
set -euo pipefail
cd "$(dirname "$0")/.."
make build >/dev/null
BIN="$PWD/adbmount"
ROOT="$(mktemp -d)"; MNT="$(mktemp -d)"
trap '"$BIN" down >/dev/null 2>&1 || true; rm -rf "$ROOT" "$MNT"' EXIT
mkdir -p "$ROOT/sdcard/Music" "$ROOT/storage/ABCD-1234" "$ROOT/storage/emulated"
echo hello > "$ROOT/sdcard/Music/a.txt"; echo card > "$ROOT/storage/ABCD-1234/c.txt"
export FAKE_ADB_ROOT="$ROOT" ADBMOUNT_DIR="$MNT" ADBMOUNT_STATE_DIR="$MNT/.state" ADBMOUNT_CONFIG="$MNT/config.json" PATH="$PWD/testdata/fakeadb:$PATH"

"$BIN" up
[ "$(cat "$MNT/android/Music/a.txt")" = hello ] || { echo "FAIL read"; exit 1; }
[ "$(cat "$MNT/sdcard/c.txt")" = card ]         || { echo "FAIL sd read"; exit 1; }
echo written > "$MNT/android/Music/new.txt"
[ "$(cat "$ROOT/sdcard/Music/new.txt")" = written ] || { echo "FAIL write-through"; exit 1; }
mv "$MNT/android/Music/new.txt" "$MNT/android/Music/renamed.txt"
[ -f "$ROOT/sdcard/Music/renamed.txt" ] || { echo "FAIL rename"; exit 1; }
rm "$MNT/android/Music/renamed.txt"
[ ! -e "$ROOT/sdcard/Music/renamed.txt" ] || { echo "FAIL delete"; exit 1; }
"$BIN" status
"$BIN" down
out="$("$BIN" status)"
case "$out" in *" mounted ("*) echo "FAIL down"; exit 1 ;; esac

"$BIN" up --ro --only android
if touch "$MNT/android/x" 2>/dev/null; then echo "FAIL ro accepted a write"; exit 1; fi
echo "mount e2e OK"
