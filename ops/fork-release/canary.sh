#!/bin/bash
set -euo pipefail
umask 077
HERE=$(cd "$(dirname "$0")" && pwd)
directory=$(readlink -f "${1:?release directory required}")
key_id=${2:?dedicated smoke API key ID required}
[[ "$key_id" =~ ^[0-9]+$ ]]
case "$directory" in /data/sub2api-patched/releases/*) ;; *) exit 64;; esac
test -f "$directory/manifest.json"
live_sha=$(sha256sum /opt/sub2api/sub2api | awk '{print $1}')
bash "$HERE/fork-remote.sh" --check "$directory" "$live_sha"
candidate_sha=$(sha256sum "$directory/sub2api" | awk '{print $1}')
if [ -e "$directory/SMOKE_VERIFIED.sha256" ]; then
  mv "$directory/SMOKE_VERIFIED.sha256" "$directory/SMOKE_VERIFIED.previous-$(date +%Y%m%d%H%M%S)"
fi
evidence=$(mktemp -d "$directory/smoke-$(date +%Y%m%d-%H%M%S).XXXXXX")
# Refuse collisions instead of stopping an unrelated service.
! ss -lnt | grep -q ':18081 '
test "$(systemctl is-active sub2api-patched-canary.service || true)" != active
cleanup() { systemctl stop sub2api-patched-canary.service || true; }
trap cleanup EXIT
# Baseline excludes migration changes. This canary shares the live DB/Redis;
# run with an explicitly selected test API key, and never enable public ingress.
systemd-run --unit=sub2api-patched-canary /bin/bash -c 'cd /opt/sub2api; set -a; . /etc/sub2api/sub2api.env; set +a; export SERVER_PORT=18081 SERVER_HOST=127.0.0.1 GATEWAY_OPENAI_ATTESTATION_MODE=http; exec "$1"' bash "$directory/sub2api"
healthy=false
for i in $(seq 1 60); do
  if curl -fsS --max-time 2 http://127.0.0.1:18081/health >/dev/null; then healthy=true; break; fi
  sleep 1
done
test "$healthy" = true
for model in gpt-5.5 gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-6-astra; do
  /usr/bin/python /data/sub2api-patched/tools/smoke-gateway.py 18081 "$model" "$key_id" | tee "$evidence/$model.jsonl"
done
test "$(sha256sum /opt/sub2api/sub2api | awk '{print $1}')" = "$live_sha"
test "$(sha256sum "$directory/sub2api" | awk '{print $1}')" = "$candidate_sha"
sha256sum "$directory/sub2api" | awk '{print $1}' > "$directory/SMOKE_VERIFIED.sha256"
echo 'SMOKE_VERIFIED: five models including GPT-5.5 with Lite, tool invocation plus follow-up; production binary unchanged'

