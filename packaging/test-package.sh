#!/bin/bash
# Run inside a disposable Debian or Rocky Linux container with packages mounted.
set -euo pipefail

package=$1
expected_version=$2
case "$package" in
  *.deb)
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq curl openssl util-linux "$package"
    ;;
  *.rpm)
    dnf install -y curl-minimal openssl util-linux diffutils "$package"
    ;;
  *) exit 2 ;;
esac

test "$(filegate version)" = "$expected_version"
test "$(/usr/bin/fg version)" = "$expected_version"
id filegate
test -f /lib/systemd/system/filegate.service
systemd-analyze verify /lib/systemd/system/filegate.service
test ! -e /etc/filegate/token
test ! -e /etc/systemd/system/multi-user.target.wants/filegate.service
runuser -u filegate -- test -r /etc/filegate/conf.yaml
openssl rand -hex 32 > /etc/filegate/token
chown root:filegate /etc/filegate/token
chmod 0640 /etc/filegate/token
runuser -u filegate -- filegate validate

setpriv --reuid=filegate --regid=filegate --init-groups filegate serve > /tmp/filegate.log 2>&1 &
daemon_pid=$!
trap 'kill "$daemon_pid" 2>/dev/null || true' EXIT
ready=0
for attempt in {1..100}; do
  if curl --fail --silent http://127.0.0.1:8080/health > /tmp/health.json; then
    ready=1
    break
  fi
  sleep 0.1
done
if [ "$ready" != 1 ]; then cat /tmp/filegate.log; exit 1; fi
runuser -u filegate -- filegate status | tee /tmp/status.json
grep -F "$expected_version" /tmp/status.json
kill -TERM "$daemon_pid"
wait "$daemon_pid"
trap - EXIT

printf '\n# Operator configuration\n' >> /etc/filegate/conf.yaml
cp /etc/filegate/conf.yaml /tmp/expected-conf.yaml
touch /srv/filegate/cloud/operator-data /var/lib/filegate/operator-state
case "$package" in
  *.deb) apt-get install -y -qq --reinstall "$package" ;;
  *.rpm) dnf reinstall -y "$package" ;;
esac
cmp /tmp/expected-conf.yaml /etc/filegate/conf.yaml
runuser -u filegate -- filegate validate
test "$(/usr/bin/fg version)" = "$expected_version"
# Exercise the package manager's service-removal hooks without a host systemd.
mkdir -p /run/systemd/system
# Package managers may reset PATH, so replace the container's executable.
mv /usr/bin/systemctl /usr/bin/systemctl.real
cat > /usr/bin/systemctl <<'EOF'
#!/bin/sh
echo "$*" >> /tmp/service-actions
EOF
chmod +x /usr/bin/systemctl
case "$package" in
  *.deb) apt-get remove -y -qq filegate ;;
  *.rpm) dnf remove -y filegate ;;
esac
grep -Fx 'stop filegate.service' /tmp/service-actions
grep -Fx 'disable filegate.service' /tmp/service-actions
test ! -e /usr/bin/filegate
test ! -L /usr/bin/fg
test -f /srv/filegate/cloud/operator-data
test -f /var/lib/filegate/operator-state
echo "Package install, daemon, reinstall, removal and data preservation passed: $package"
