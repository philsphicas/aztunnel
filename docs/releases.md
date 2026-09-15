# Releases

## Weekly maintenance releases

**Approve the weekly workflow when you want to publish.** Every Tuesday at
16:17 UTC, **Prepare Maintenance Release** selects the current `main` commit,
requires its newest push-to-main CI run to have succeeded, and proposes the next
patch version. In the workflow run, review the version and source comparison,
then click **Review deployments** and approve `stable-release`.

Approval creates an immutable version tag on that exact commit. **Stable Release**
then builds and tests it, publishes the binaries and container images, and creates
the GitHub release with generated release notes. Changes merged while approval
is pending are not silently included; cancel and start a new run to include them.
The approved job also runs its tagging script from that exact source revision,
refreshing remote `main` and stable tags before checking ancestry and creating a tag.
Development releases continue independently after successful `main` builds.

Each approved week gets a new version even without source changes. Stable builds
pull fresh container bases, disable the build cache, and upgrade Azure Linux
runtime packages.
This picks up merged dependency updates and available container security fixes
without maintaining a separate package inventory or guessing whether a rebuild
is worthwhile. It does not guarantee that every vulnerability has an upstream fix.

There are no release PRs, release-request JSON files, commit-message classifiers,
or separate approval workflows. The release does not infer semantic versioning:
review the included changes before approving the proposed patch.

## Urgent releases and version changes

Run **Actions > Prepare Maintenance Release > Run workflow** on `main` at any time.
The default is `patch`; choose `minor` or `major` when the changes require it.
The same approval applies. Reject or cancel an incorrect pending proposal and
start a new run with the desired bump.

Only one maintenance run proceeds at a time. A tag awaiting publication blocks
another release. If another stable release advances the baseline while approval
is pending, the old proposal fails rather than publishing an outdated version.

## Setup

The `stable-release` GitHub environment must require maintainer approval and
allow deployments only from the `main` branch. In this repository it is configured
with `philsphicas` as the required reviewer and administrator bypass disabled.
Self-review is allowed so the maintainer can approve manually dispatched runs.
Keep this protection in place: an environment name in YAML alone does not
require approval. Forks must configure equivalent protection before enabling
the schedule.

The tag job uses the existing `GH_AUTOMATION_TOKEN` repository secret. It needs
repository access with Contents and Workflows read/write (tags can contain workflow
changes), plus Actions read to verify CI. The same credential is also used by
Dependabot and Go updater automation, which need Pull requests read/write.
Do not substitute `GITHUB_TOKEN` for tag creation or Dependabot auto-merges:
those writes would not trigger the downstream workflows.
Ensure the credential is available to Dependabot-triggered workflows; where
Dependabot secrets are used, configure `GH_AUTOMATION_TOKEN` there too.

## Recovery and publication safety

If preparation reports missing, pending, or failed CI, wait for successful
push-to-main CI on the source and rerun preparation. Successful PR checks alone
are not sufficient. For a historical Dependabot merge made with `GITHUB_TOKEN`,
merge the automation fix normally, wait for CI on the new `main` tip, then start
preparation again; rerunning preparation cannot create the missing CI run.

If approval or tagging fails before a tag is created, rerun the failed job.
If the version tag already exists, follow or rerun **Stable Release** instead.
Tags are never moved, and published stable versions are never overwritten.
Publication retries reuse existing container candidates and exact-version image
digests; they do not rebuild an already-published version with different contents.
Container refreshes always receive a new version.

Direct `vMAJOR.MINOR.PATCH` tag pushes remain an intentional maintainer escape
hatch and bypass the weekly environment approval. The stable workflow still
validates main ancestry, builds, and tests the tagged commit. Version components
must have no leading zeros and must be safe integers (at most `9007199254740991`).
Cancel a pending maintenance run before pushing a release tag manually: direct
pushes do not participate in the maintenance workflow's concurrency lock.

When migrating from the old process, cancel any pending old maintenance runs and
close obsolete `automation/maintenance-release` PRs rather than merging them.
The old request file no longer controls publication.
