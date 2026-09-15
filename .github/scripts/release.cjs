const { execFileSync } = require("node:child_process");
const fs = require("node:fs");

const stablePattern = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;

function run(command, args) {
  return execFileSync(command, args, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
  }).trim();
}

function isStableVersion(tag) {
  const match = typeof tag === "string" && stablePattern.exec(tag);
  return Boolean(
    match &&
    match[0] === tag &&
    match.slice(1).every((part) => Number.isSafeInteger(Number(part))),
  );
}

function parseVersion(tag) {
  if (!isStableVersion(tag)) {
    throw new Error(
      `Invalid stable version: ${tag}. Use vMAJOR.MINOR.PATCH without leading zeros; components must be safe integers.`,
    );
  }
  return tag.slice(1).split(".").map(Number);
}

function compareVersions(left, right) {
  const a = parseVersion(left);
  const b = parseVersion(right);
  return a[0] - b[0] || a[1] - b[1] || a[2] - b[2];
}

function latestVersion(tags) {
  const versions = tags.filter(isStableVersion).sort(compareVersions);
  if (!versions.length) throw new Error("No valid stable version found.");
  return versions.at(-1);
}

function nextVersion(previous, bump) {
  const parts = parseVersion(previous);
  const index = ["major", "minor", "patch"].indexOf(bump);
  if (index < 0) throw new Error(`Invalid bump: ${bump}`);
  parts[index]++;
  parts.fill(0, index + 1);
  const version = `v${parts.join(".")}`;
  parseVersion(version);
  return version;
}

function validateImageMatrix(matrix) {
  if (!Array.isArray(matrix?.include) || matrix.include.length === 0) {
    throw new Error("Release image matrix must contain at least one image.");
  }
  const ids = new Set();
  const targets = new Set();
  for (const image of matrix.include) {
    if (
      !image ||
      typeof image.id !== "string" ||
      !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(image.id)
    ) {
      throw new Error("Release image IDs must be lowercase kebab-case.");
    }
    for (const field of ["image_suffix", "variant"]) {
      if (
        typeof image[field] !== "string" ||
        !/^(?:-[a-z0-9][a-z0-9._-]*)?$/.test(image[field])
      ) {
        throw new Error(`Invalid ${field} for release image ${image.id}.`);
      }
    }
    for (const field of ["file", "builder_variant", "runtime"]) {
      if (typeof image[field] !== "string" || !/^[^\s|]+$/.test(image[field])) {
        throw new Error(
          `Missing or invalid ${field} for release image ${image.id}.`,
        );
      }
    }
    if (
      image.file.startsWith("/") ||
      image.file.includes("\\") ||
      image.file.split("/").includes("..")
    ) {
      throw new Error(
        `Dockerfile path for ${image.id} must be repository-relative.`,
      );
    }
    if (
      image.builder_repository !== undefined &&
      (typeof image.builder_repository !== "string" ||
        !/^[^\s|]+$/.test(image.builder_repository))
    ) {
      throw new Error(
        `Invalid builder_repository for release image ${image.id}.`,
      );
    }
    if (
      image.allow_missing_dev !== undefined &&
      typeof image.allow_missing_dev !== "boolean"
    ) {
      throw new Error(`allow_missing_dev for ${image.id} must be a boolean.`);
    }
    const target = `${image.image_suffix}|${image.variant}`;
    if (ids.has(image.id) || targets.has(target)) {
      throw new Error(`Duplicate release image ID or tag target: ${image.id}.`);
    }
    ids.add(image.id);
    targets.add(target);
  }
  return matrix.include;
}

function validateSource(sha) {
  if (
    typeof sha !== "string" ||
    !/^[0-9a-f]{40}$/.test(sha) ||
    sha.length !== 40
  ) {
    throw new Error(
      "Invalid release source SHA; use a full 40-character commit SHA.",
    );
  }
}

async function latestStable(github, repo) {
  const releases = await github.paginate(github.rest.repos.listReleases, {
    ...repo,
    per_page: 100,
  });
  const versions = releases
    .filter(
      (release) =>
        !release.draft &&
        !release.prerelease &&
        /^v\d+\.\d+\.\d+$/.test(release.tag_name),
    )
    .map((release) => {
      // Noncanonical published versions require repair, not a lower baseline.
      parseVersion(release.tag_name);
      return release.tag_name;
    });
  if (!versions.length) {
    throw new Error(
      "Publish an initial stable release before enabling maintenance releases.",
    );
  }
  return latestVersion(versions);
}

async function requireCI(github, repo, sha) {
  const { data } = await github.rest.actions.listWorkflowRuns({
    ...repo,
    workflow_id: "ci.yml",
    branch: "main",
    event: "push",
    head_sha: sha,
    per_page: 1,
  });
  // GitHub returns newest first. Never search older runs for a success.
  const latest = data.workflow_runs[0];
  if (
    !latest ||
    latest.head_sha !== sha ||
    latest.head_branch !== "main" ||
    latest.event !== "push"
  ) {
    throw new Error(
      `Missing main push CI run for ${sha}; run CI before releasing.`,
    );
  }
  if (latest.status !== "completed") {
    throw new Error(
      `Pending main push CI for ${sha} (${latest.status}); wait for it to succeed.`,
    );
  }
  if (latest.conclusion !== "success") {
    throw new Error(
      `Failed main push CI for ${sha} (${latest.conclusion}); the newest run must succeed.`,
    );
  }
}

function requireNoPendingTag(previous, execute = run, requested) {
  const tags = execute("git", ["tag", "--list", "v*"])
    .split(/\r?\n/)
    .filter(isStableVersion);
  const pending = tags.filter(
    (tag) => tag !== requested && compareVersions(tag, previous) > 0,
  );
  if (pending.length) {
    throw new Error(
      `Newer stable tag(s) ${pending.join(", ")} have not finished publishing. Finish or repair that release first.`,
    );
  }
  return tags;
}

async function prepare({
  github,
  context,
  core,
  execute = run,
  env = process.env,
}) {
  const sourceSHA = execute("git", ["rev-parse", "HEAD"]);
  validateSource(sourceSHA);
  const previous = await latestStable(github, context.repo);
  const version = nextVersion(previous, env.RELEASE_BUMP ?? "patch");
  requireNoPendingTag(previous, execute);
  await requireCI(github, context.repo, sourceSHA);
  const outputs = { version, source_sha: sourceSHA, previous_tag: previous };
  for (const [key, value] of Object.entries(outputs))
    core.setOutput(key, value);
  const repositoryURL = `https://github.com/${context.repo.owner}/${context.repo.repo}`;
  await core.summary
    .addRaw(
      `## Proposed maintenance release ${version}\n\n` +
        `Immutable source: [\`${sourceSHA}\`](${repositoryURL}/commit/${sourceSHA})\n\n` +
        `[Compare ${previous} to the proposed source](${repositoryURL}/compare/${previous}...${sourceSHA})\n\n` +
        "Approve the stable-release environment to tag this source. Publication rebuilds images with fresh bases and OS packages.\n",
    )
    .write();
}

async function tag({
  github,
  context,
  core,
  execute = run,
  env = process.env,
}) {
  const version = env.RELEASE_VERSION;
  const sourceSHA = env.RELEASE_SOURCE_SHA;
  const previous = env.RELEASE_PREVIOUS_TAG;
  parseVersion(version);
  parseVersion(previous);
  validateSource(sourceSHA);
  if (compareVersions(version, previous) <= 0) {
    throw new Error(
      "Release version must be newer than the previous stable baseline.",
    );
  }
  try {
    execute("git", ["merge-base", "--is-ancestor", sourceSHA, "HEAD"]);
  } catch (error) {
    throw new Error(
      "Release source is not an ancestor of current main, or ancestry could not be verified.",
      { cause: error },
    );
  }
  await requireCI(github, context.repo, sourceSHA);
  if ((await latestStable(github, context.repo)) !== previous) {
    throw new Error(
      "Published stable baseline changed; prepare and approve a new release.",
    );
  }
  const tags = requireNoPendingTag(previous, execute, version);
  if (tags.includes(version)) {
    if (execute("git", ["rev-parse", `${version}^{commit}`]) !== sourceSHA) {
      throw new Error(
        "Existing tag points to a different source; it will not be moved.",
      );
    }
    core.notice(
      `Tag ${version} already exists. Follow or rerun its Stable Release workflow; the tag will not be moved or recreated.`,
    );
    return;
  }
  await github.rest.git.createRef({
    ...context.repo,
    ref: `refs/tags/${version}`,
    sha: sourceSHA,
  });
  core.notice(
    `Created ${version} at ${sourceSHA}. Follow Stable Release for publication.`,
  );
}

module.exports = {
  compareVersions,
  isStableVersion,
  latestVersion,
  nextVersion,
  parseVersion,
  prepare,
  requireCI,
  requireNoPendingTag,
  tag,
  validateImageMatrix,
};

if (require.main === module) {
  switch (process.argv[2]) {
    case "image-matrix":
      console.log(
        JSON.stringify({
          include: validateImageMatrix(
            JSON.parse(fs.readFileSync(".github/release-images.json", "utf8")),
          ),
        }),
      );
      break;
    case "validate-version":
      parseVersion(process.argv[3]);
      break;
    case "latest-version":
      console.log(latestVersion(fs.readFileSync(0, "utf8").split(/\r?\n/)));
      break;
    default:
      throw new Error(
        "Usage: release.cjs image-matrix | validate-version <tag> | latest-version (tags on stdin)",
      );
  }
}
