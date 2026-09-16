#!/bin/sh
set -eu

ETC_DIR="${FILEGATE_ETC_DIR:-/etc/filegate}"
STATE_DIR="${FILEGATE_STATE_DIR:-/var/lib/filegate}"
DATA_DIR="${FILEGATE_DATA_DIR:-/srv/filegate/cloud}"
LOG_DIR="${FILEGATE_LOG_DIR:-/var/log/filegate}"
BINDIR="${FILEGATE_BINDIR:-/usr/bin}"

print_fg_alias_snippet() {
  cat <<'EOF'
Add this to your shell config to use the short Filegate command:
  alias fg='filegate'
EOF
}

passwd_field() {
  user="$1"
  field="$2"
  getent passwd "${user}" 2>/dev/null | awk -F: -v field="${field}" '{ print $field }'
}

install_fg_alias() {
  target_home="${HOME:-}"
  target_shell="${SHELL:-}"

  current_uid="$(id -u 2>/dev/null || echo "")"
  if [ "${current_uid}" = "0" ] && [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
    sudo_home="$(passwd_field "${SUDO_USER}" 6 || true)"
    sudo_shell="$(passwd_field "${SUDO_USER}" 7 || true)"
    if [ -n "${sudo_home}" ]; then
      target_home="${sudo_home}"
    fi
    if [ -n "${sudo_shell}" ]; then
      target_shell="${sudo_shell}"
    fi
  fi

  if [ -z "${target_home}" ] || [ ! -d "${target_home}" ]; then
    print_fg_alias_snippet
    return 0
  fi

  case "$(basename "${target_shell}")" in
    bash)
      rc_file="${target_home}/.bashrc"
      ;;
    zsh)
      rc_file="${target_home}/.zshrc"
      ;;
    *)
      print_fg_alias_snippet
      return 0
      ;;
  esac

  touch "${rc_file}"
  if grep -Eq "^[[:space:]]*alias[[:space:]]+fg=" "${rc_file}"; then
    echo "fg alias already exists in ${rc_file}"
    return 0
  fi

  {
    printf '\n'
    printf '# Filegate CLI\n'
    printf "alias fg='filegate'\n"
  } >>"${rc_file}"

  echo "Added fg alias to ${rc_file}"
}

install_fg_command() {
  if [ -e "${BINDIR}/filegate" ]; then
    ln -sf filegate "${BINDIR}/fg"
  fi
}

mkdir -p "${ETC_DIR}" "${STATE_DIR}" "${DATA_DIR}" "${LOG_DIR}"
chmod 0750 "${STATE_DIR}" "${DATA_DIR}" "${LOG_DIR}"
install_fg_command

if id -u filegate >/dev/null 2>&1; then
  chown filegate:filegate "${STATE_DIR}" "${DATA_DIR}" "${LOG_DIR}"
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "${FILEGATE_INSTALL_ALIAS_FG:-}" = "1" ]; then
  install_fg_alias
fi

exit 0
