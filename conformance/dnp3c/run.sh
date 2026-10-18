#!/usr/bin/env bash
# Runs dnp3c's conformance procedures against go-dnp3's outstation.
#
#   conformance/dnp3c/run.sh                 # 3.1r1, subset level 3, every procedure
#   conformance/dnp3c/run.sh 2.7             # 2.7, subset level 2
#   conformance/dnp3c/run.sh 3.1r1 8.11      # one section or procedure
#   KEEP_GOING=1 conformance/dnp3c/run.sh    # run past failures, and summarise
#
# It builds the device under test (dut/) and the plugin (plugin/), starts the
# DUT, and runs dnp3c with the plugin in a JVM. The report lands in
# results/<revision>/L<level>/report.html.
#
# Java 17 and Maven are used from the PATH when both are there, and otherwise
# from the maven:3.9-eclipse-temurin-17 image, on the host's network so it can
# reach the DUT on 127.0.0.1.
#
# DNP3C_HOST and DNP3C_CONTROL_PORT move the DUT off 127.0.0.1:20000 and its
# control port off 20080, so two revisions can run at once.
set -euo pipefail

REV="${1:-3.1r1}"
RUN="${2:-all}"
HOST="${DNP3C_HOST:-127.0.0.1}"
CONTROL="127.0.0.1:${DNP3C_CONTROL_PORT:-20080}"

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'kill "$DUT_PID" 2>/dev/null || true; wait "$DUT_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
DUT_PID=

case "$REV" in
  2.7) LEVEL=2 ;;
  3.1r1) LEVEL=3 ;;
  *) echo "unknown revision $REV (2.7 or 3.1r1)" >&2; exit 2 ;;
esac

# device.ini is for level 3; a later line overrides an earlier one.
CONFIG="$HERE/results/device-$REV.ini"
mkdir -p "$HERE/results"
cp "$HERE/device.ini" "$CONFIG"
echo "outstation.subset.level = $LEVEL" >>"$CONFIG"
echo "tcp.device.address = $HOST" >>"$CONFIG"
if [ -n "${DNP3C_CAPTURE:-}" ]; then
  # Capture needs root; CI sets this, a local run normally does not.
  echo "tcp.capture.adapter = $DNP3C_CAPTURE" >>"$CONFIG"
fi

echo "==> building the device under test"
(cd "$ROOT" && go build -o "$WORK/dut" ./conformance/dnp3c/dut)

if command -v java >/dev/null && command -v mvn >/dev/null; then
  mvn_() { (cd "$HERE/plugin" && mvn -q -B "$@"); }
  java_() { (cd "$HERE" && java "$@"); }
  PLUGIN="$HERE/plugin"
else
  command -v docker >/dev/null || { echo "need Java 17 and Maven, or Docker" >&2; exit 1; }
  IMAGE=maven:3.9-eclipse-temurin-17
  M2="${DNP3C_M2:-$HOME/.m2}"
  mkdir -p "$M2"
  docker_() {
    docker run --rm --network host -u "$(id -u):$(id -g)" \
      -e MAVEN_CONFIG=/m2 -e DUT_CONTROL="${DUT_CONTROL:-}" \
      -v "$M2:/m2" -v "$HERE:/work" -w "$1" "$IMAGE" "${@:2}"
  }
  mvn_() { docker_ /work/plugin mvn -q -B -Dmaven.repo.local=/m2/repository "$@"; }
  java_() { docker_ /work java "$@"; }
  PLUGIN=/work/plugin
  CONFIG="/work/results/device-$REV.ini"
fi

echo "==> building the plugin"
mvn_ compile dependency:build-classpath -Dmdep.outputFile=target/classpath.txt
CP="$PLUGIN/target/classes:$(cat "$HERE/plugin/target/classpath.txt")"

echo "==> starting the device under test"
"$WORK/dut" -listen "$HOST:20000" -control "$CONTROL" -level "$LEVEL" >"$HERE/results/dut-$REV.log" 2>&1 &
DUT_PID=$!
for _ in $(seq 50); do
  if (exec 3<>"/dev/tcp/${CONTROL%:*}/${CONTROL#*:}") 2>/dev/null; then break; fi
  sleep 0.1
done

echo "==> running dnp3c $REV ($RUN)"
export DUT_CONTROL="http://$CONTROL"
MODE=()
[ -n "${KEEP_GOING:-}" ] && MODE=(-keep-going)
java_ -cp "$CP" godnp3.conformance.Main "${MODE[@]}" -rev "$REV" -config "$CONFIG" -run "$RUN"
