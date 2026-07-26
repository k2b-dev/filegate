#!/usr/bin/env bash
# Many-small-files upload benchmark.
#
# Brings up an isolated Filegate stack (own ports, own volumes, own TLS edge)
# and runs the load generator from a sibling container on the same Docker
# network, so host port forwarding does not dominate the numbers.
#
# The stack is recreated between configurations on purpose. Uploading a corpus
# leaves tens of thousands of files and index rows behind, and reusing that
# state makes later runs measure a different server than earlier ones; early
# trials varied by 3x from that alone. Every run therefore starts from an empty
# mount and an empty index.
#
# Results land in bench/results/tree-bench-<ts>.csv.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

PROJECT="${FILEGATE_BENCH_PROJECT:-filegate-tree-bench}"
COMPOSE_FILE="bench/compose.bench.yml"
NETWORK="${PROJECT}_default"
TOKEN="bench-token"
HOST_HEALTH_URL="${FILEGATE_BENCH_HEALTH_URL:-http://127.0.0.1:4911/health}"
BIN_DIR="$ROOT_DIR/bench/.bin"

LOGS_SCALE="${FILEGATE_BENCH_LOGS_SCALE:-0.25}"
NM_SCALE="${FILEGATE_BENCH_NM_SCALE:-0.25}"
PHOTOS_SCALE="${FILEGATE_BENCH_PHOTOS_SCALE:-0.25}"
PRESET="${FILEGATE_BENCH_PRESET:-full}"
REPEATS="${FILEGATE_BENCH_REPEATS:-2}"

mkdir -p bench/results "$BIN_DIR"
TS="$(date +%Y%m%d_%H%M%S)"
CSV_HOST="bench/results/tree-bench-${TS}.csv"
CSV_IN_CONTAINER="/results/tree-bench-${TS}.csv"

echo "building the load generator"
docker run --rm \
  -v "$ROOT_DIR":/src \
  -v filegate-bench-gomod:/go/pkg/mod \
  -v filegate-bench-gocache:/root/.cache/go-build \
  -w /src \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  golang:1.25 go build -o /src/bench/.bin/filegate-bench ./cmd/filegate-bench

echo "building the server image"
docker compose -p "$PROJECT" -f "$COMPOSE_FILE" build >/dev/null

cleanup() {
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

reset_stack() {
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d >/dev/null
  for _ in $(seq 1 60); do
    if curl -fsS "$HOST_HEALTH_URL" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  curl -fsS "$HOST_HEALTH_URL" >/dev/null
}

bench() {
  local label="$1"
  shift
  for run in $(seq 1 "$REPEATS"); do
    reset_stack
    echo "--- ${label} (run ${run}/${REPEATS})"
    docker run --rm \
      --network "$NETWORK" \
      -v "$BIN_DIR":/opt/bench:ro \
      -v "$ROOT_DIR/bench/results":/results \
      alpine:3 /opt/bench/filegate-bench \
      --mode tree \
      --token "$TOKEN" \
      --path-base data/bench \
      --output-csv "$CSV_IN_CONTAINER" \
      --tree-label "$label" \
      "$@"
  done
}

direct_url=(--base-url http://filegate:8080)
edge_url=(--base-url https://edge --insecure)

# The first corpus after a cold Docker host runs ~3x slower than the same
# corpus a few minutes later, so one throwaway load goes first and its result
# is never recorded.
echo "--- warm-up (discarded)"
reset_stack
docker run --rm \
  --network "$NETWORK" \
  -v "$BIN_DIR":/opt/bench:ro \
  alpine:3 /opt/bench/filegate-bench \
  --mode tree --token "$TOKEN" --path-base data/bench "${direct_url[@]}" \
  --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32 \
  --tree-label warmup >/dev/null

# 1. Per-file overhead: which upload path, and how much parallelism helps.
if [[ "$PRESET" == "full" || "$PRESET" == "levers" ]]; then
  bench put-c1 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 1
  bench put-c8 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 8
  bench put-c32 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32
  bench put-c64 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 64
  bench put-c128 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 128

  # 2. Keep-alive: the cost of a fresh TCP connection per request.
  bench put-c8-nokeepalive "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 8 --keep-alive=false
  bench put-c32-nokeepalive "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32 --keep-alive=false

  # 3. Signed direct upload: one extra round trip per file to mint the URL.
  bench direct-c32 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport direct --tree-files 32

  # 4. Sessions, and what batched session creation is worth.
  bench session-c32-batch1 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 1
  bench session-c32-batch32 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 32
  bench session-c32-batch100 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100
  bench session-c64-batch100 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 64 --tree-segments 64 --tree-batch 100
fi

# 4b. Commit cost on its own.
#
# The narrow set for "did commit stop dominating", small enough to run twice in
# one sitting -- once per build under comparison. Host drift on this machine
# reaches 2.4x over tens of minutes, so a before/after claim is only worth
# making when both arms ran back to back, and that rules out the full preset.
# The two put rows are the large-file regression check.
if [[ "$PRESET" == "commit" ]]; then
  bench session-c32-batch100 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100
  bench nm-session-c32-batch100 "${direct_url[@]}" --tree-shape node-modules --tree-scale "$NM_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100
  bench photos-session-c8-seg32 "${direct_url[@]}" --tree-shape photos --tree-scale "$PHOTOS_SCALE" --tree-transport session --tree-files 8 --tree-hash 4 --tree-segments 16 --tree-segment-size 33554432 --tree-batch 32
  bench put-c32 "${direct_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32
  bench photos-put-c8 "${direct_url[@]}" --tree-shape photos --tree-scale "$PHOTOS_SCALE" --tree-transport put --tree-files 8
fi

# 5. HTTP/1.1 versus HTTP/2, both through the same TLS edge.
if [[ "$PRESET" == "full" || "$PRESET" == "http2" ]]; then
  bench edge-h1-put-c32 "${edge_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32
  bench edge-h2-put-c32 "${edge_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport put --tree-files 32 --http2
  bench edge-h1-session-c32 "${edge_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100
  bench edge-h2-session-c32 "${edge_url[@]}" --tree-shape logs --tree-scale "$LOGS_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100 --http2
fi

# 6. The other two shapes from the ticket.
if [[ "$PRESET" == "full" || "$PRESET" == "shapes" ]]; then
  bench nm-put-c32 "${direct_url[@]}" --tree-shape node-modules --tree-scale "$NM_SCALE" --tree-transport put --tree-files 32
  bench nm-session-c32-batch100 "${direct_url[@]}" --tree-shape node-modules --tree-scale "$NM_SCALE" --tree-transport session --tree-files 32 --tree-segments 32 --tree-batch 100

  bench photos-put-c8 "${direct_url[@]}" --tree-shape photos --tree-scale "$PHOTOS_SCALE" --tree-transport put --tree-files 8
  bench photos-session-c8-seg8 "${direct_url[@]}" --tree-shape photos --tree-scale "$PHOTOS_SCALE" --tree-transport session --tree-files 8 --tree-hash 4 --tree-segments 8 --tree-segment-size 8388608 --tree-batch 32
  bench photos-session-c8-seg32 "${direct_url[@]}" --tree-shape photos --tree-scale "$PHOTOS_SCALE" --tree-transport session --tree-files 8 --tree-hash 4 --tree-segments 16 --tree-segment-size 33554432 --tree-batch 32
fi

echo "wrote $CSV_HOST"
