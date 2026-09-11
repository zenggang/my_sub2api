# Fork release packages

This is the fork path alongside the unchanged legacy official-tag + Lite packager.
Build only from a clean `release == origin/release`. No release tag is created.
The remote scripts reuse the legacy backup, atomic binary replacement, service
health and automatic recovery sequence, with these additional checks:

- Exact source archive, embedded binary commit/version, test events and checksums.
- All bundled SQL migrations must already be present in the live database.
  Historical checksums are accepted only using the source runner's named rules.
  Pending SQL or unknown checksum differences block even candidate startup.
- All Redis account concurrency keys are checked, plus Guard active requests.
- HTTP attestation is enabled using a dedicated systemd drop-in; binary and this
  drop-in are backed up and restored together on deployment failure/rollback.

Set `GO_BIN`, `PNPM_BIN` and Node PATH from the local deployment profile, then:

```bash
python3 -B -m unittest discover -s ops/fork-release -p 'test_*.py'
bash -n ops/fork-release/fork-remote.sh ops/fork-release/canary.sh
python3 ops/fork-release/build.py "$SUB2API_BUILD_ROOT"
```

The builder prints `BUILT`, an immutable package directory containing binary,
manifest, source archive, build log, named test events and SHA256SUMS. It executes
frozen-lockfile frontend installation/build and unit-tagged Lite/attestation/WS
tests before the Linux amd64 embedded build. Copy the tarball to a new directory
under `/data/sub2api-patched/releases`, extract there, and check SHA256SUMS.
Upload `verify.py`, `fork-remote.sh`, `canary.sh` to a new versioned tool directory.
Do not replace the legacy `/usr/local/bin/sub2api-patched-release`.

```bash
bash TOOL_DIR/fork-remote.sh --check PACKAGE_DIR LIVE_SHA
bash TOOL_DIR/canary.sh PACKAGE_DIR TEST_KEY_ID
bash TOOL_DIR/fork-remote.sh --deploy PACKAGE_DIR LIVE_SHA
bash TOOL_DIR/fork-remote.sh --rollback BACKUP_DIR CURRENT_SHA
```

Paths/SHA/key IDs are values established per run. The canary uses the existing
remote smoke script, port 18081 and `mode=http`; it shares DB/Redis and therefore
generates usage and normal request state. It is not a read-only DB sandbox.
Its SHA-bound smoke marker is required by deployment. Deployment checks remain
mandatory even after canary completion. Do not use `--allow-active` without the
user explicitly authorizing interruption. A pre-switch check is not zero downtime.

The first fork rollout has no new SQL pending: both 238 migrations were already
applied during the September 11 candidate run. The earlier claim that those
candidates did not change the DB was incorrect; current live checks must decide
future migration readiness. Rollback restores binary/configuration only, not SQL.

After deployment use the selected test key through port 8080 to verify tool
results and response.completed; read usage for actual model/account and confirm
the executable SHA and effective process attestation mode. Save a release record
under ignored docs-local. A binary health response alone is not acceptance.
