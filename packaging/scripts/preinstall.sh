#!/bin/sh
set -eu

if command -v systemctl >/dev/null 2>&1; then
  if systemctl is-active --quiet filegate.service >/dev/null 2>&1; then
    cat >&2 <<'EOF'
filegate.service is currently running.

Stop the service before installing or upgrading Filegate:
  sudo systemctl stop filegate

Then rerun the package install/upgrade command and start the service again.
EOF
    exit 1
  fi
fi

if ! getent group filegate >/dev/null 2>&1; then
  groupadd --system filegate
fi

if ! id -u filegate >/dev/null 2>&1; then
  NOLOGIN="$(command -v nologin || true)"
  if [ -z "${NOLOGIN}" ]; then
    NOLOGIN="/usr/sbin/nologin"
  fi
  useradd \
    --system \
    --gid filegate \
    --home /var/lib/filegate \
    --create-home \
    --shell "${NOLOGIN}" \
    --comment "Filegate service user" \
    filegate
fi

exit 0
