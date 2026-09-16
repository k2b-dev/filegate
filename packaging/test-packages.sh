#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo 'Unsupported test host architecture' >&2; exit 1 ;;
esac
version=$(jq -r .version dist/metadata.json)
for image in debian:12-slim debian:13-slim rockylinux/rockylinux:9 rockylinux/rockylinux:10; do
  case "$image" in debian:*) format=deb ;; *) format=rpm ;; esac
  docker run --rm -v "$PWD/dist:/packages:ro" -v "$PWD/packaging:/tests:ro" \
    "$image" bash /tests/test-package.sh "/packages/filegate_linux_${arch}.${format}" "$version"
done
