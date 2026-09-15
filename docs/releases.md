# Releases

## The maintainer's job

**Merge the maintenance release PR when you want to publish.** The bot proposes
the version, records the exact source commit and container inputs, and writes
the release notes. Merging creates the version tag and starts the existing
Stable Release workflow. There is no second approval or manual tag command.

Preparation runs every Tuesday at 16:17 UTC. It updates one PR on
`automation/maintenance-release` rather than opening a new PR every week.
No unreleased shipping changes and no container updates means no release PR.
Development releases continue to follow successful `main` builds independently.

The PR releases its recorded source commit, **not** its eventual merge commit.
Code merged after preparation waits for the next release. The recorded source
must be on `main`, and its newest main CI run must have succeeded.
Approval also verifies that the request's last mainline change came from a
merged `automation/maintenance-release` PR in this repository targeting `main`.
An unrelated PR or direct edit of the request file does not publish a release.
This is an accidental-release safeguard, not a replacement for branch permissions.

## When a version decision is needed

Routine `fix:`, `perf:`, `revert:`, `chore(deps):`, and `chore(go):` commits
affecting shipped files produce a patch candidate. Docs, CI-only changes, and
test code do not independently trigger one. Module manifests and checksums are
included conservatively, even in the e2e modules: release builds use the Go
workspace, so those dependency updates can change versions linked into shipped
binaries. This favors an occasional extra patch over missing a security update.
Dependabot explicitly uses the `chore(deps)` commit-message prefix for every
configured ecosystem, so routine update classification does not depend on
its inferred commit style.

Features, breaking-change declarations, and unrecognized commit titles produce
a **draft** PR. Run **Actions > Prepare Maintenance Release > Run workflow** on
`main` and choose `patch`, `minor`, or `major` to make the version decision.
The same PR is refreshed and marked ready; then merge it as usual.
An explicit `feat:` requires at least a minor bump. A declared breaking change
requires a minor bump before v1, or a major bump after v1. An ambiguous title
can explicitly be classified as a patch when it really is backward compatible.
Simply marking the draft ready does not bypass these rules.

For an urgent security fix, use **Run workflow** immediately instead of waiting
for Tuesday. `auto` still handles routine patches; select an explicit bump for
anything needing a version decision. `force` prepares a release even when the
source and observed container inputs are unchanged.
That force intent survives scheduled refreshes while the pending request still
targets the same source and previous stable release. Close the pending PR to
cancel it; force intent is not inherited by a different source or baseline.

## Containers without source changes

The preparation workflow compares upstream builder and runtime image digests
against the previous approved release request. The shared image list is
`.github/release-images.json`, also used by the publishing workflow.
The same validated matrix controls artifact collection and both stable and
development promotion. Missing or unexpected image artifacts fail publication
before any channel changes. When introducing a variant without a `dev` tag,
set `allow_missing_dev: true` on that matrix entry; other missing development
tags remain errors. This keeps initial-tag creation explicit and preserves
the existing rollback behavior.
It additionally installs/upgrades Azure Linux runtime packages on **amd64 and
arm64** and compares the RPM inventories. This catches OpenSSL and other OS
updates that arrive in package repositories before the base image tag changes.
Registry or package-manager failures fail preparation; they are not treated
as "nothing changed."

Stable publication pulls fresh bases, disables the Docker layer cache, and
upgrades Azure Linux OS packages before installing OpenSSL and certificates.
An existing stable candidate is reused by digest on retries rather than rebuilt
against newer upstream contents. Promotion preflights every exact-version image
tag: matching tags are left untouched, and any different digest fails the run
before tag writes. Rolling minor/latest tags can then be repaired on a retry
without changing already-promoted exact versions. Registry failures never count
as evidence that an image is absent.
Container-only refreshes receive a **new version**; published stable releases
are never rebuilt in place. The observations in the PR are change-detection
inputs, not a promise that mutable upstream repositories are frozen between
preparation and publication, and not an SBOM of the resulting images.

The first maintenance release establishes the container baseline. A manually
tagged release that does not match the recorded request also causes the next
preparation to establish a new baseline. This can deliberately produce one
extra refresh rather than incorrectly assume containers are current.

## Setup and recovery

The workflows reuse `GH_AUTOMATION_TOKEN`. It must have access to this repository
with Contents and Pull requests read/write, Workflows read/write for creating
workflow-bearing tags, and Actions read. This is the existing automation
credential, not a new secret. Do not replace it with `GITHUB_TOKEN`: PRs and tags
created by that token do not trigger the required downstream workflows.
Dependabot auto-merge also uses `GH_AUTOMATION_TOKEN` so its merges trigger
push-to-main CI; approval still uses `GITHUB_TOKEN`. Eligible updates fail
explicitly before approval if the automation credential is unavailable.
Ensure the credential is available to Dependabot-triggered workflows; where
Dependabot secrets are used, configure the same `GH_AUTOMATION_TOKEN` there.

No new GitHub environment is required. Keep normal required PR checks enabled.
The workflows do **not** enable auto-merge or approve their own release PRs.
To remove the human later, enable auto-merge for eligible release PRs after
required checks; keep drafts and explicit version decisions as the exception.

If preparation fails because the newest main CI run has not passed, rerun it
after CI succeeds. If there is no push-to-main CI run because an older
Dependabot workflow merged with `GITHUB_TOKEN`, rerunning preparation cannot
repair it. Merge this workflow fix normally with a maintainer credential, wait
for CI on the new `main` tip, and rerun preparation. Successful PR checks alone
do not satisfy the exact-source main CI gate.
If tagging fails, rerun **Approve Maintenance Release**.
That recovery path finds the request's introducing commit even if unrelated
commits have advanced `main`, and supports merge, squash, and rebase merges.
GitHub's commit-to-PR association can briefly lag a merge; rerun approval if
that lookup has not caught up yet. With no request file, approval reports that
there is nothing to do.
If a tag already exists, follow or rerun its **Stable Release** run instead:
tagging retries never move the tag or start a second publication.
A pending tag or merged release request blocks further preparation until
publication finishes, preventing multiple releases from racing for a version.
If a newer stable release overtakes an open candidate, run preparation again;
it will refresh the candidate against that release or close it if unnecessary.

Manual strict `vMAJOR.MINOR.PATCH` tags remain supported for deliberate releases.
All release paths use the same validation: no leading zeros, and each component
must be at most `9007199254740991` (JavaScript's largest safe integer). Invalid
unpublished tags are ignored when selecting the newest stable tag; an existing
published noncanonical numeric version fails preparation explicitly rather
than silently selecting an older release.
Automatic preparation is a convenience, not a guarantee that every security
advisory is relevant, every fix is available, or a fresh image is vulnerability-free.
