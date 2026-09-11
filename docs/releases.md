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

## When a version decision is needed

Routine `fix:`, `perf:`, `revert:`, `chore(deps):`, and `chore(go):` commits
affecting shipped files produce a patch candidate. Docs, CI-only changes, and
test code do not independently trigger one. Module manifests and checksums are
included conservatively, even in the e2e modules: release builds use the Go
workspace, so those dependency updates can change versions linked into shipped
binaries. This favors an occasional extra patch over missing a security update.

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

## Containers without source changes

The preparation workflow compares upstream builder and runtime image digests
against the previous approved release request. The shared image list is
`.github/release-images.json`, also used by the publishing workflow.
It additionally installs/upgrades Azure Linux runtime packages on **amd64 and
arm64** and compares the RPM inventories. This catches OpenSSL and other OS
updates that arrive in package repositories before the base image tag changes.
Registry or package-manager failures fail preparation; they are not treated
as "nothing changed."

Stable publication pulls fresh bases, disables the Docker layer cache, and
upgrades Azure Linux OS packages before installing OpenSSL and certificates.
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

No new GitHub environment is required. Keep normal required PR checks enabled.
The workflows do **not** enable auto-merge or approve their own release PRs.
To remove the human later, enable auto-merge for eligible release PRs after
required checks; keep drafts and explicit version decisions as the exception.

If preparation fails because the newest main CI run has not passed, rerun it
after CI succeeds. If tagging fails, rerun **Approve Maintenance Release**.
If a tag already exists, follow or rerun its **Stable Release** run instead:
tagging retries never move the tag or start a second publication.
A pending tag or merged release request blocks further preparation until
publication finishes, preventing multiple releases from racing for a version.
If a newer stable release overtakes an open candidate, run preparation again;
it will refresh the candidate against that release or close it if unnecessary.

Manual strict `vMAJOR.MINOR.PATCH` tags remain supported for deliberate releases.
Automatic preparation is a convenience, not a guarantee that every security
advisory is relevant, every fix is available, or a fresh image is vulnerability-free.
