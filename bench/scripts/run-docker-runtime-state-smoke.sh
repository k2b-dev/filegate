#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
suffix="$$"
container="filegate-runtime-state-smoke-${suffix}"
image="filegate-runtime-state-smoke:${suffix}"
data_volume="${container}-data"
index_volume="${container}-index"
config_volume="${container}-config"

cleanup() {
  docker rm -f "${container}" >/dev/null 2>&1 || true
  docker volume rm "${data_volume}" "${index_volume}" "${config_volume}" >/dev/null 2>&1 || true
  docker image rm "${image}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

start_container() {
  # The smoke targets runtime-store persistence. Root keeps the fresh Linux
  # data volume writable; production bind-mount ownership is operator-managed.
  docker run --detach \
    --name "${container}" \
    --user 0:0 \
    --publish 127.0.0.1::8080 \
    --env FILEGATE_SERVER_LISTEN=:8080 \
    --env FILEGATE_STORAGE_BASE_PATHS=/data \
    --env FILEGATE_STORAGE_INDEX_PATH=/var/lib/filegate/index \
    --env FILEGATE_STORAGE_RUNTIME_CONFIG_PATH=/var/lib/filegate/config \
    --volume "${data_volume}:/data" \
    --volume "${index_volume}:/var/lib/filegate/index" \
    --volume "${config_volume}:/var/lib/filegate/config" \
    "${image}" serve >/dev/null

  local port
  port="$(docker port "${container}" 8080/tcp | awk -F: 'NR == 1 { print $NF }')"
  base_url="http://127.0.0.1:${port}"
  for _ in {1..80}; do
    if curl --fail --silent --show-error "${base_url}/health" >/dev/null 2>&1; then
      return
    fi
    sleep 0.25
  done
  docker logs "${container}" >&2
  echo "filegate did not become healthy" >&2
  return 1
}

assert_runtime_state() {
  local config_json keys_json
  config_json="$(curl --fail --silent --show-error \
    --header "Authorization: Bearer ${token}" \
    "${base_url}/v1/config")"
  if [[ "${config_json}" != *'"path":"server.access_log_enabled","effective":false,"desired":false'* ]]; then
    echo "applied runtime manifest state is missing" >&2
    return 1
  fi

  keys_json="$(curl --fail --silent --show-error \
    --header "Authorization: Bearer ${token}" \
    "${base_url}/v1/s3/keys")"
  if [[ "${keys_json}" != *'"accessKey":"runtime-smoke"'* ]]; then
    echo "runtime S3 key is missing" >&2
    return 1
  fi
}

docker build --quiet --tag "${image}" "${repo_root}" >/dev/null
start_container

token=""
while IFS= read -r line; do
  if [[ "${line}" =~ ([A-Z2-7]{40}) ]]; then
    token="${BASH_REMATCH[1]}"
    break
  fi
done < <(docker logs "${container}" 2>&1)
if [[ -z "${token}" ]]; then
  docker logs "${container}" >&2
  echo "first boot did not emit a generated API token" >&2
  exit 1
fi

curl --fail --silent --show-error \
  --request POST \
  --header "Authorization: Bearer ${token}" \
  --header "Content-Type: application/json" \
  --data '{"values":{"server.access_log_enabled":false},"expectedRevision":""}' \
  "${base_url}/v1/config/apply" >/dev/null

curl --fail --silent --show-error \
  --request POST \
  --header "Authorization: Bearer ${token}" \
  --header "Content-Type: application/json" \
  --data '{"accessKey":"runtime-smoke","buckets":["data"]}' \
  "${base_url}/v1/s3/keys" >/dev/null

assert_runtime_state
docker rm -f "${container}" >/dev/null
start_container

if docker logs "${container}" 2>&1 | grep --quiet "generated an API token"; then
  echo "container recreation generated a replacement API token" >&2
  exit 1
fi
assert_runtime_state

echo "docker runtime state: generated token, manifest, and S3 key survived recreation"
