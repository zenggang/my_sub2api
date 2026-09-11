# Repository workflow

This fork follows [docs/FORK_WORKFLOW.md](docs/FORK_WORKFLOW.md). Read it before changing branches, implementing changes, syncing upstream, or preparing a contribution.

Before compiling a release binary, packaging, staging, validating a candidate, deploying, or rolling back, read [docs/RELEASING.md](docs/RELEASING.md) and the local `docs-local/DEPLOYMENT_PROFILE.md`. Reuse their established toolchain and commands; inspect only facts that can change (source SHA, tool compatibility, migrations, live SHA, active requests, and selected smoke credentials). Do not rediscover or invent a release procedure on each run. The currently documented legacy packager builds an official tag plus a pinned patch, not arbitrary fork `release` HEAD; never mislabel its output as a fork build. Record each run using [docs/RELEASE_RECORD_TEMPLATE.md](docs/RELEASE_RECORD_TEMPLATE.md). A missing local profile requires resolving deployment-specific values, not blocking ordinary development tests.

## Branch roles

- `upstream` is `Wei-Shaw/sub2api`; `origin` is `zenggang/my_sub2api`.
- `main` follows official `upstream/main`. Do not put fork-specific code, documentation, merges from `release`, or development commits on `main`.
- `release` is the fork's integration branch. All feature, fix, and maintenance development branches start from the current `release`.
- Implement and verify changes on a development branch, then integrate into `release`. Preserve published history; do not force-push or reset shared branches.
- Keep the normal workspace on `release` or a development branch. Prefer a separate worktree for updating `main` so active work stays intact.

## Upstream synchronization

- When the user requests a main/trunk sync, fetch official `upstream/main`, reconcile any `origin/main` advance, fast-forward local `main`, and push the result to `origin/main`.
- Treat `mian` as `main` when the user's context is a main/trunk sync.
- Confirm the remote URLs, branch ancestry, worktree state, and final local/remote SHAs. If `main` contains fork-only commits or the history diverges, stop that synchronization and report the divergence; do not overwrite it or merge unrelated history automatically.
- Updating `main` does not authorize merging it into `release`, creating a release tag, or deploying. Integrate `main` into `release` when the user requests that integration, using a branch created from `release` and appropriate regression/migration checks.

## Official pull requests

- A request to develop or test a change is not permission to submit it upstream.
- Before creating any PR, including a draft, against the official repository, obtain explicit user approval for the specific contribution. Do not sign a CLA or post approval/comments on the user's behalf without authorization.
- Develop on a branch from `release`. Prepare the official contribution by exporting only the reviewed generic commits onto a clean branch based on `upstream/main`; this is contribution packaging, not a different development base.
- Keep fork-only workflow files, private deployment material, internal diagnostics, account configuration, and credentials out of the official contribution. Re-run relevant checks against its actual upstream base.
- The user can authorize implementation, fork integration/push, official PR submission, and deployment separately. Execute the authorized scope without repeatedly requesting permission already given.

## Verification and delivery

- Preserve unrelated dirty changes. Stage explicit paths and review the staged diff before committing.
- Match tests to the change. Documentation-only changes need Git/diff/link checks; application or merge changes need relevant tests, including migration review when applicable.
- Report the starting/final branches and SHAs, tests performed, whether commits were pushed, and whether an official PR or deployment occurred.
- Use ignored `docs-local/` for environment-specific diagnostics. Never include credentials or raw device attestations in commits or logs.
- A `release` branch commit identifies integrated source, not proof that the code has been deployed or passed a production business checkpoint.
