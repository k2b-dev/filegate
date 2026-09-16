#!/bin/sh
set -eu

# Debian passes "remove"; RPM passes 0 when the last package is removed.
case "${1:-}" in
  remove|0)
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
      systemctl stop filegate.service
      systemctl disable filegate.service
    fi
    ;;
esac
