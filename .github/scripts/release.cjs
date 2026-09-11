const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { isDeepStrictEqual } = require("node:util");

const requestPath = ".github/release.json";
const releaseBranch = "automation/maintenance-release";
const stablePattern = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const shaPattern = /^[0-9a-f]{40}$/;
const digestPattern = /^sha256:[0-9a-f]{64}$/;

function run(command, args) {
  return execFileSync(command, args, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
  }).trim();
}

function isStableVersion(tag) {
  const match = stablePattern.exec(tag);
  return (
    match !== null &&
    match.slice(1).every((part) => Number.isSafeInteger(Number(part)))
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

function ships(file) {
  // Workspace resolution can change linked dependencies even when only an
  // e2e module's manifest changes. Conservatively include module inputs.
  if (/(^|\/)go\.(mod|sum)$/.test(file)) return true;
  if (
    /^(docs|e2e|scripts)\//.test(file) ||
    /(^|\/)(testdata|\.github\/scripts)\//.test(file) ||
    /(?:_test\.go|\.md)$/.test(file)
  ) {
    return false;
  }
  if (file.startsWith(".github/")) {
    return (
      file === ".github/release-images.json" ||
      file === ".github/workflows/publish-release.yml" ||
      file.startsWith(".github/actions/package-release/")
    );
  }
  return (
    file === ".dockerignore" ||
    file === "Makefile" ||
    file === "LICENSE" ||
    /^(go\.(mod|sum|work|work\.sum)|Dockerfile[^/]*|cmd\/.*|internal\/.*|mockrelay\/.*)$/.test(
      file,
    )
  );
}

function classify(commits, previous) {
  let minimum = "patch";
  let needsDecision = false;
  const changes = commits.filter((commit) => commit.files.some(ships));
  for (const { message } of changes) {
    const title = message.split("\n")[0];
    if (
      /^[a-z]+(?:\([^)]+\))?!:/.test(title) ||
      /^BREAKING[ -]CHANGE:/m.test(message)
    ) {
      minimum = parseVersion(previous)[0] === 0 ? "minor" : "major";
      needsDecision = true;
    } else if (/^feat(?:\([^)]+\))?:/.test(title)) {
      if (minimum !== "major") minimum = "minor";
      needsDecision = true;
    } else if (
      !/^(?:fix|perf|revert)(?:\([^)]+\))?:|^chore\((?:deps|go)\):/.test(title)
    ) {
      needsDecision = true;
    }
  }
  return { changes, minimum, needsDecision };
}

function plan({
  previous,
  sourceSHA,
  commits,
  inputs,
  baseline,
  bump = "auto",
  force = false,
}) {
  parseVersion(previous);
  if (!shaPattern.test(sourceSHA))
    throw new Error("Source must be a full commit SHA.");
  if (!["auto", "patch", "minor", "major"].includes(bump)) {
    throw new Error(`Invalid bump: ${bump}`);
  }
  const { changes, minimum, needsDecision } = classify(commits, previous);
  const inputsChanged = !baseline || !isDeepStrictEqual(inputs, baseline);
  if (changes.length === 0 && !inputsChanged && !force) return null;
  const selected =
    bump === "auto"
      ? needsDecision && minimum === "patch"
        ? "minor"
        : minimum
      : bump;
  const ranks = { patch: 0, minor: 1, major: 2 };
  if (ranks[selected] < ranks[minimum]) {
    throw new Error(`These changes require at least a ${minimum} bump.`);
  }
  return {
    version: nextVersion(previous, selected),
    previous_tag: previous,
    source_sha: sourceSHA,
    bump: selected,
    explicit_bump: bump !== "auto",
    decision_required: bump === "auto" && needsDecision,
    force,
    container_inputs: inputs,
    reasons: [
      ...changes.map(
        ({ sha, message }) => `${sha.slice(0, 12)} ${message.split("\n")[0]}`,
      ),
      ...(inputsChanged
        ? [
            baseline
              ? "Container base images or OS packages changed."
              : "Establish container input baseline.",
          ]
        : []),
      ...(force
        ? ["Maintainer requested a release even without detected changes."]
        : []),
    ],
  };
}

// Match the runtime package operations in Dockerfile.azurelinux3. The inventory
// catches repository updates even when the Azure Linux base tag hasn't moved.
const packageProbe =
  "tdnf upgrade -y >&2 && " +
  "tdnf install -y openssl-libs ca-certificates >&2 && " +
  "rpm -qa --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}\\n' | LC_ALL=C sort";

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

function observeInputs(matrix, goWork, execute = run) {
  const definitions = validateImageMatrix(matrix);
  const version = /^toolchain go(1\.\d+\.\d+)\s*$/m.exec(goWork)?.[1];
  if (!version)
    throw new Error("go.work must declare an exact stable Go toolchain.");
  const references = new Set();
  for (const image of definitions) {
    references.add(
      `${image.builder_repository || "golang"}:${version}-${image.builder_variant}`,
    );
    if (image.runtime !== "scratch") references.add(image.runtime);
  }
  const images = {};
  for (const reference of [...references].sort()) {
    const output = execute("docker", [
      "buildx",
      "imagetools",
      "inspect",
      reference,
    ]);
    const digest = /^Digest:\s+(sha256:[0-9a-f]{64})\s*$/m.exec(output)?.[1];
    if (!digest) throw new Error(`No valid manifest digest for ${reference}.`);
    images[reference] = digest;
  }
  const packages = {};
  const azure = definitions.find((image) => image.id === "client-azurelinux3");
  if (azure) {
    for (const platform of ["linux/amd64", "linux/arm64"]) {
      const inventory = execute("docker", [
        "run",
        "--rm",
        "--platform",
        platform,
        `${azure.runtime}@${images[azure.runtime]}`,
        "sh",
        "-ec",
        packageProbe,
      ])
        .split("\n")
        .filter(Boolean)
        .sort();
      if (
        !inventory.some((rpm) => rpm.startsWith("openssl-libs-")) ||
        !inventory.some((rpm) => rpm.startsWith("ca-certificates-"))
      ) {
        throw new Error(
          `Incomplete Azure Linux package inventory for ${platform}.`,
        );
      }
      packages[platform] = inventory;
    }
  }
  return { images, azurelinux_packages: packages };
}

function readCommits(previous, sourceSHA, execute = run) {
  execute("git", ["merge-base", "--is-ancestor", previous, sourceSHA]);
  const revisions = execute("git", [
    "rev-list",
    "--first-parent",
    "--reverse",
    `${previous}..${sourceSHA}`,
  ]);
  if (!revisions) return [];
  return revisions.split("\n").map((sha) => ({
    sha,
    message: execute("git", ["show", "-s", "--format=%B", sha]),
    files: execute("git", ["diff", "--name-only", "-z", `${sha}^`, sha])
      .split("\0")
      .filter(Boolean),
  }));
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
      // Previously published noncanonical versions need explicit repair, not
      // silent exclusion from the maintenance release baseline.
      parseVersion(release.tag_name);
      return release.tag_name;
    })
    .sort(compareVersions);
  if (!versions.length)
    throw new Error(
      "Publish an initial stable release before enabling maintenance releases.",
    );
  return versions.at(-1);
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
  const latest = data.workflow_runs[0];
  if (!latest || latest.head_sha !== sha || latest.conclusion !== "success") {
    throw new Error(
      `The newest main CI run for ${sha} must succeed before releasing it.`,
    );
  }
}

function requireNoPendingTag(previous, execute = run) {
  const tags = execute("git", ["tag", "--list", "v*"])
    .split("\n")
    .filter(isStableVersion);
  if (tags.some((tag) => compareVersions(tag, previous) > 0)) {
    throw new Error(
      "A newer stable tag has not finished publishing. Finish or repair that release first.",
    );
  }
}

function describe(request, repository) {
  return [
    `## Release ${request.version}`,
    "",
    request.decision_required
      ? "**Version decision needed.** Run Prepare Maintenance Release on main with an explicit patch, minor, or major bump. Do not just mark this PR ready."
      : "**Merge this PR to publish.** No separate tag or release approval is needed.",
    "",
    `Source: [\`${request.source_sha}\`](https://github.com/${repository}/commit/${request.source_sha})`,
    `Previous stable: \`${request.previous_tag}\``,
    "",
    "Only this source commit is tagged. Changes merged later are not included.",
    "",
    "### Release notes",
    "",
    ...request.reasons.map((reason) => `- ${reason}`),
    "",
    "Container observations are compared with the previous approved release. Publication pulls fresh bases and installs fresh OS packages; these observations are not an artifact SBOM.",
    "",
    "This PR is updated in place. It is never auto-merged by this workflow.",
    "",
  ].join("\n");
}

async function prepare({
  github,
  context,
  core,
  execute = run,
  files = fs,
  env = process.env,
}) {
  const repo = context.repo;
  const previous = await latestStable(github, repo);
  requireNoPendingTag(previous, execute);
  const sourceSHA = execute("git", ["rev-parse", "HEAD"]);
  await requireCI(github, repo, sourceSHA);
  const old = files.existsSync(requestPath)
    ? JSON.parse(files.readFileSync(requestPath, "utf8"))
    : null;
  if (old && compareVersions(old.version, previous) > 0) {
    throw new Error(
      "An approved request is awaiting publication. Finish or repair that release first.",
    );
  }
  const releasedSHA = execute("git", ["rev-parse", `${previous}^{commit}`]);
  const baseline =
    old?.version === previous && old.source_sha === releasedSHA
      ? old.container_inputs
      : null;
  const inputs = observeInputs(
    JSON.parse(files.readFileSync(".github/release-images.json", "utf8")),
    files.readFileSync("go.work", "utf8"),
    execute,
  );
  const { data: open } = await github.rest.pulls.list({
    ...repo,
    state: "open",
    base: "main",
    head: `${repo.owner}:${releaseBranch}`,
  });
  let bump = env.RELEASE_BUMP || "auto";
  let force = env.RELEASE_FORCE === "true";
  // Preserve explicit intent only while the same source and release baseline apply.
  if (open.length) {
    const { data } = await github.rest.repos.getContent({
      ...repo,
      path: requestPath,
      ref: open[0].head.sha,
    });
    const pending = JSON.parse(
      Buffer.from(data.content, "base64").toString("utf8"),
    );
    if (pending.previous_tag === previous && pending.source_sha === sourceSHA) {
      if (bump === "auto" && pending.explicit_bump) bump = pending.bump;
      force = force || pending.force === true;
    }
  }
  const request = plan({
    previous,
    sourceSHA,
    inputs,
    baseline,
    bump,
    commits: readCommits(previous, sourceSHA, execute),
    force,
  });
  core.setOutput("ready", Boolean(request));
  if (!request) {
    if (open.length) {
      await github.rest.issues.createComment({
        ...repo,
        issue_number: open[0].number,
        body: `Closing this superseded candidate: ${previous} covers the current release inputs.`,
      });
      await github.rest.pulls.update({
        ...repo,
        pull_number: open[0].number,
        state: "closed",
      });
    }
    await core.summary
      .addRaw(
        "No unreleased shipping changes or container updates. No release PR needed.",
      )
      .write();
    return;
  }
  files.writeFileSync(requestPath, `${JSON.stringify(request, null, 2)}\n`);
  const bodyPath = path.join(env.RUNNER_TEMP, "maintenance-release-body.md");
  const body = describe(request, `${repo.owner}/${repo.repo}`);
  files.writeFileSync(bodyPath, body);
  core.setOutput("body_path", bodyPath);
  core.setOutput("version", request.version);
  core.setOutput("decision_required", request.decision_required);
  await core.summary.addRaw(body).write();
}

function validateRequest(request, previous, commits) {
  if (request.previous_tag !== previous)
    throw new Error("Release request is stale; prepare it again.");
  if (!shaPattern.test(request.source_sha))
    throw new Error("Invalid release source SHA.");
  if (request.decision_required !== false)
    throw new Error("Choose an explicit version bump before publishing.");
  if (typeof request.explicit_bump !== "boolean")
    throw new Error("Missing explicit_bump flag.");
  if (request.version !== nextVersion(previous, request.bump))
    throw new Error("Release version does not match its bump.");
  const expected = plan({
    previous,
    sourceSHA: request.source_sha,
    commits,
    inputs: request.container_inputs,
    baseline: null,
    bump: request.explicit_bump ? request.bump : "auto",
  });
  if (expected.decision_required || expected.version !== request.version) {
    throw new Error("Shipping changes need an explicit version decision.");
  }
  if (
    !request.container_inputs ||
    !Object.keys(request.container_inputs.images || {}).length ||
    Object.values(request.container_inputs.images).some(
      (digest) => !digestPattern.test(digest),
    )
  ) {
    throw new Error("Missing or invalid container input observations.");
  }
}

async function requireReleaseProvenance(github, repo, execute) {
  // First-parent follows the mainline introduction for merge, squash, and rebase
  // merges, including recovery runs after unrelated commits advanced main.
  const commit = execute("git", [
    "log",
    "--first-parent",
    "-1",
    "--format=%H",
    "--",
    requestPath,
  ]);
  if (!shaPattern.test(commit))
    throw new Error(`No mainline commit introduced ${requestPath}.`);
  const { data: associated } =
    await github.rest.repos.listPullRequestsAssociatedWithCommit({
      ...repo,
      commit_sha: commit,
      per_page: 100,
    });
  const repository = `${repo.owner}/${repo.repo}`.toLowerCase();
  if (
    !associated.some(
      (pr) =>
        pr.merged_at &&
        pr.base?.ref === "main" &&
        pr.head?.ref === releaseBranch &&
        pr.head?.repo?.full_name?.toLowerCase() === repository,
    )
  ) {
    throw new Error(
      `${commit} did not arrive through a merged, same-repository ${releaseBranch} PR targeting main. ` +
        "If it just merged, retry after GitHub updates the commit association; otherwise prepare a release PR or use a manual stable tag.",
    );
  }
}

async function approve({ github, context, core, execute = run, files = fs }) {
  const repo = context.repo;
  if (!files.existsSync(requestPath)) {
    core.notice("No release request on main; nothing to approve.");
    return;
  }
  const request = JSON.parse(files.readFileSync(requestPath, "utf8"));
  parseVersion(request.version);
  if (!shaPattern.test(request.source_sha))
    throw new Error("Invalid release source SHA.");
  await requireReleaseProvenance(github, repo, execute);
  const previous = await latestStable(github, repo);
  // A successful rerun must not move a tag or rebuild a published release.
  if (previous === request.version) {
    const sha = execute("git", ["rev-parse", `${request.version}^{commit}`]);
    if (sha !== request.source_sha)
      throw new Error("Published version points to a different source.");
    core.notice(`${request.version} is already published.`);
    return;
  }
  execute("git", ["merge-base", "--is-ancestor", request.source_sha, "HEAD"]);
  validateRequest(
    request,
    previous,
    readCommits(previous, request.source_sha, execute),
  );
  await requireCI(github, repo, request.source_sha);
  const tags = execute("git", ["tag", "--list", "v*"])
    .split("\n")
    .filter(isStableVersion);
  if (tags.some((tag) => compareVersions(tag, request.version) > 0)) {
    throw new Error("A newer stable tag superseded this request.");
  }
  if (tags.includes(request.version)) {
    const sha = execute("git", ["rev-parse", `${request.version}^{commit}`]);
    if (sha !== request.source_sha)
      throw new Error("Existing tag points to a different source.");
    core.notice(
      `Tag ${request.version} already exists. Follow or rerun its Stable Release workflow; the tag will not be moved.`,
    );
    return;
  }
  await github.rest.git.createRef({
    ...repo,
    ref: `refs/tags/${request.version}`,
    sha: request.source_sha,
  });
  await core.summary
    .addRaw(
      `Created \`${request.version}\` at \`${request.source_sha}\`. ` +
        `Follow [Stable Release](https://github.com/${repo.owner}/${repo.repo}/actions/workflows/release.yml) for publication.`,
    )
    .write();
}

module.exports = {
  approve,
  classify,
  compareVersions,
  describe,
  isStableVersion,
  latestVersion,
  nextVersion,
  observeInputs,
  packageProbe,
  parseVersion,
  plan,
  prepare,
  readCommits,
  requireCI,
  requireNoPendingTag,
  ships,
  validateImageMatrix,
  validateRequest,
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
