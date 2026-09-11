#!/bin/bash
# Fork deployment, derived from the established Lite release switch/rollback flow.
# Requires a verified fork package; refuses pending or unknown SQL migrations.
set -euo pipefail
umask 077
ROOT=/data/sub2api-patched
HERE=$(cd "$(dirname "$0")" && pwd)
DROPIN=/etc/systemd/system/sub2api.service.d/90-fork-attestation.conf
PROGRAM=/opt/sub2api/sub2api
mode=${1:---help}
if [ "$mode" = --help ]; then
  echo 'Usage: fork-remote.sh --check|--deploy RELEASE_DIR EXPECTED_LIVE_SHA [--allow-active]'
  echo '       fork-remote.sh --rollback BACKUP_DIR EXPECTED_LIVE_SHA [--allow-active]'
  exit 0
fi
case "$mode" in --check|--deploy|--rollback) ;; *) exit 64;; esac
test $# -ge 3
directory=$(readlink -f "$2")
expected_live=$3
case "$directory" in "$ROOT"/releases/*|"$ROOT"/backups/*) ;; *) echo invalid_release_directory; exit 64;; esac
[[ "$expected_live" =~ ^[0-9a-f]{64}$ ]]
allow_active=${4:-}
[[ -z "$allow_active" || "$allow_active" = --allow-active ]]
test "$(id -u)" = 0
exec 9>/var/lock/sub2api-auto-update.lock
flock -n 9 || { echo another_release_is_running; exit 1; }
actual_live=$(sha256sum "$PROGRAM" | awk '{print $1}')
test "$actual_live" = "$expected_live" || { echo stale_live_sha; exit 1; }
systemctl is-active --quiet sub2api.service
curl -fsS --max-time 5 http://127.0.0.1:18080/health >/dev/null
if [ "$mode" = --rollback ]; then
  candidate="$directory/sub2api.before"
  wanted=$(awk '{print $1}' "$directory/before.sha256")
else
  candidate="$directory/sub2api"
  wanted=$(/usr/bin/python "$HERE/verify.py" "$directory")
fi
test -f "$candidate" && test ! -L "$candidate"
test "$(sha256sum "$candidate" | awk '{print $1}')" = "$wanted"
file "$candidate" | grep -q 'ELF 64-bit.*x86-64'
if [ "$mode" != --rollback ]; then
  version=$(/usr/bin/python -c 'import json,sys;print(json.load(open(sys.argv[1]))["build_version"])' "$directory/manifest.json")
  "$candidate" --version 2>&1 | grep -F "Sub2API $version " >/dev/null
fi
echo "CANDIDATE_VERIFIED sha=$wanted live_sha=$actual_live"
if [ "$mode" = --check ]; then exit 0; fi
if [ "$mode" = --deploy ]; then
  # Real candidate smoke and review are an explicit release checkpoint.
  test -f "$directory/SMOKE_VERIFIED.sha256" || { echo candidate_smoke_required; exit 1; }
  test "$(tr -d '\r\n' < "$directory/SMOKE_VERIFIED.sha256")" = "$wanted" || { echo stale_smoke_evidence; exit 1; }
fi
if [ "$allow_active" != --allow-active ]; then
  active=$(curl -fsS --max-time 5 --unix-socket /run/sub2api-max-guard/control.sock http://localhost/api/state | /usr/bin/python -c 'import json,sys;print(json.load(sys.stdin)["metrics"]["active_requests"])')
  test "$active" = 0 || { echo "active_requests=$active; retry later or explicitly use --allow-active"; exit 1; }
  task_slots=$(/opt/redis/bin/redis-cli --scan --pattern 'concurrency:account:*')
  while IFS= read -r task_slot; do
    test -z "$task_slot" && continue
    test "$(/opt/redis/bin/redis-cli --raw ZCARD "$task_slot")" = 0 || { echo "active_account_slot=$task_slot"; exit 1; }
  done <<< "$task_slots"
fi
# Save rollback target before creating a new backup.
rollback_source=$directory
backup=$(mktemp -d "$ROOT/backups/release-$(date +%Y%m%d-%H%M%S).XXXXXX")
cp -p "$PROGRAM" "$backup/sub2api.before"
if test -e "$DROPIN"; then cp -p "$DROPIN" "$backup/attestation.conf"; else touch "$backup/attestation.absent"; fi
restore_mode() {
  if test -f "$1/attestation.conf"; then
    mkdir -p "$(dirname "$DROPIN")"
    install -m 644 "$1/attestation.conf" "$DROPIN"
  elif test -f "$1/attestation.absent" && test -e "$DROPIN"; then
    mv "$DROPIN" "$backup/removed-attestation.conf"
  fi
  systemctl daemon-reload
}
sha256sum "$backup/sub2api.before" > "$backup/before.sha256"
cp -p /opt/sub2api/data/config.yaml "$backup/config.yaml"
cp -p /etc/sub2api/sub2api.env "$backup/runtime.env"
chmod 600 "$backup/config.yaml" "$backup/runtime.env"
sha256sum /opt/sub2api/data/config.yaml /etc/sub2api/sub2api.env > "$backup/config.sha256"
sudo -u postgres /opt/postgresql/bin/psql -h /var/run/postgresql -d sub2api -X -At -c "select json_agg(x) from (select id,credentials->'model_mapping' mapping from accounts where deleted_at is null and platform='openai' order by id) x" > "$backup/mappings.json"
ready() {
  local i
  for i in $(seq 1 60); do
    if systemctl is-active --quiet sub2api.service && curl -fsS --max-time 2 http://127.0.0.1:18080/health >/dev/null && curl -fsS --max-time 2 http://127.0.0.1:8080/health >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}
recover() {
  trap - ERR
  install -m 755 "$backup/sub2api.before" "$PROGRAM.recover"
  mv -f "$PROGRAM.recover" "$PROGRAM"
  restore_mode "$backup"
  systemctl restart sub2api.service
  if ready && test "$(sha256sum "$PROGRAM" | awk '{print $1}')" = "$actual_live"; then echo "ROLLED_BACK backup=$backup"; else echo "ROLLBACK_FAILED backup=$backup"; fi
  exit 1
}
trap recover ERR
systemctl stop sub2api-auto-update.timer
systemctl disable sub2api-auto-update.timer
install -m 755 "$candidate" "$PROGRAM.patched-new"
test "$(sha256sum "$PROGRAM.patched-new" | awk '{print $1}')" = "$wanted"
mv -f "$PROGRAM.patched-new" "$PROGRAM"
if [ "$mode" = --deploy ]; then
  mkdir -p "$(dirname "$DROPIN")"
  printf '[Service]\nEnvironment=GATEWAY_OPENAI_ATTESTATION_MODE=http\n' > "$DROPIN"
  systemctl daemon-reload
else
  restore_mode "$rollback_source"
fi
since_time=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart sub2api.service
ready
for round in 1 2 3; do
  curl -fsS --max-time 5 http://127.0.0.1:18080/health >/dev/null
  curl -fsS --max-time 5 http://127.0.0.1:8080/health >/dev/null
  sleep 2
done
if journalctl -u sub2api.service --since "$since_time" --no-pager | grep -Ei 'panic|migration.*(fail|error)|listen.*(fail|error)' > "$backup/critical-startup.log"; then
  recover
fi
test "$(sha256sum "$PROGRAM" | awk '{print $1}')" = "$wanted"
sha256sum -c "$backup/config.sha256" >/dev/null
sudo -u postgres /opt/postgresql/bin/psql -h /var/run/postgresql -d sub2api -X -At -c "select json_agg(x) from (select id,credentials->'model_mapping' mapping from accounts where deleted_at is null and platform='openai' order by id) x" > "$backup/mappings-after.json"
cmp "$backup/mappings.json" "$backup/mappings-after.json"
if [ "$mode" = --deploy ]; then
  task_pid=$(systemctl show sub2api.service -p MainPID | cut -d= -f2)
  /usr/bin/python - "$task_pid" <<'PY'
import sys
env=open('/proc/'+sys.argv[1]+'/environ','rb').read().split(b'\0')
assert b'GATEWAY_OPENAI_ATTESTATION_MODE=http' in env
print('RUNTIME_ATTESTATION_MODE=http')
PY
fi
trap - ERR
echo "INSTALLED sha=$wanted backup=$backup; verify real traffic before declaring business success"
