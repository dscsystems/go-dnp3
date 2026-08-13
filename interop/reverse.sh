#!/usr/bin/env bash
# Drive the other implementations' masters against our outstation.
#
# This is the direction that finds the interesting problems. A master we wrote
# asks for what we know how to answer; somebody else's master asks for what the
# standard says it may — which is how the missing RECORD_CURRENT_TIME support
# turned up.
#
# Their demos are interactive, so this feeds them a scripted session and greps
# the result rather than asserting on it. Read the output.
set -uo pipefail

cd "$(dirname "$0")/.."
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"; kill %1 2>/dev/null' EXIT

echo "==> building our outstation"
go build -o "$OUT/dnp3-outstation" ./cmd/dnp3-outstation

echo "==> starting it on 0.0.0.0:20000"
"$OUT/dnp3-outstation" -listen 0.0.0.0:20000 -q >"$OUT/ours.log" 2>&1 &
sleep 2

# --- opendnp3's master ---------------------------------------------------
# Its menu: i = integrity scan, c = send a CROB, e = event scan, x = exit.
echo
echo "==> opendnp3 master-demo -> our outstation"
( printf 'i\n'; sleep 5; printf 'c\n'; sleep 3; printf 'e\n'; sleep 3; printf 'x\n'; sleep 2 ) \
  | timeout 90 docker run --rm -i --network host go-dnp3-interop-opendnp3 \
      /src/opendnp3/build/cpp/examples/master/master-demo 2>&1 \
  | head -c 400000 >"$OUT/opendnp3.log"

echo "--- object headers it parsed ---"
grep -oE '[0-9]{3},[0-9]{3} [A-Za-z ]+' "$OUT/opendnp3.log" | sort -u | head -20
echo "--- internal indications it saw ---"
grep -oE 'IIN: \[[^]]*\]' "$OUT/opendnp3.log" | sort -u | head
echo "--- errors ---"
grep -iE 'error|malformed|invalid' "$OUT/opendnp3.log" | head -5 || echo "  none"
