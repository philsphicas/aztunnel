const assert = require("node:assert/strict");
const { execFileSync, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");
const {
  compareVersions,
  isStableVersion,
  latestVersion,
  nextVersion,
  parseVersion,
  prepare,
  tag,
  validateImageMatrix,
} = require("./release.cjs");
const {
  candidateState,
  developmentImageEntries,
  inspectImage,
  promoteStableImages,
  readImageRefs,
} = require("./release-images.cjs");

const sha = "a".repeat(40);
const oldSHA = "b".repeat(40);
const digest = `sha256:${"c".repeat(64)}`;
const matrix = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "release-images.json"), "utf8"),
);

test("strict versions sort numerically and bump with resets", () => {
  assert.deepEqual(parseVersion("v0.4.0"), [0, 4, 0]);
  for (const value of [
    "v01.2.3",
    "1.2.3",
    "v1.2",
    "v1.2.3-rc.1",
    "v1.2.3\n",
    "dev",
    "v999999999999999999999.0.0",
    undefined,
    null,
    123,
  ]) {
    assert.throws(() => parseVersion(value));
  }
  assert.ok(compareVersions("v0.10.0", "v0.9.9") > 0);
  assert.equal(nextVersion("v0.4.9", "patch"), "v0.4.10");
  assert.equal(nextVersion("v0.4.9", "minor"), "v0.5.0");
  assert.equal(nextVersion("v0.4.9", "major"), "v1.0.0");
  assert.throws(() => nextVersion("v0.4.0", "auto"));
});

test("manual publication and tag selection share canonical safe-integer validation", () => {
  const valid = ["v0.0.0", "v0.4.0", "v1.2.3", "v9007199254740991.0.0"];
  const invalid = [
    "v01.2.3",
    "v1.02.3",
    "v1.2.03",
    "v9007199254740992.0.0",
    "v0.4.1-rc.1",
    "v1.2.3\n",
    "dev",
  ];
  const script = path.join(__dirname, "release.cjs");
  for (const version of [...valid, ...invalid]) {
    const result = spawnSync(
      process.execPath,
      [script, "validate-version", version],
      { encoding: "utf8" },
    );
    assert.ifError(result.error);
    assert.equal(result.status === 0, valid.includes(version), version);
    assert.equal(isStableVersion(version), valid.includes(version), version);
    if (!valid.includes(version))
      assert.match(result.stderr, /Invalid stable version/);
  }
  assert.equal(latestVersion(["v0.9.0", "v0.10.0", ...invalid]), "v0.10.0");
  const result = spawnSync(process.execPath, [script, "latest-version"], {
    input: [
      "v0.9.0",
      "v0.10.0",
      ...invalid.filter((value) => !value.includes("\n")),
      "",
    ].join("\r\n"),
    encoding: "utf8",
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), "v0.10.0");
  assert.throws(() => latestVersion(invalid), /No valid stable version/);
  for (const [previous, bump] of [
    ["v0.0.9007199254740991", "patch"],
    ["v0.9007199254740991.0", "minor"],
    ["v9007199254740991.0.0", "major"],
  ])
    assert.throws(() => nextVersion(previous, bump), /Invalid stable version/);
});

test("shared image matrix validates its definitions and CLI output", () => {
  assert.equal(validateImageMatrix(matrix), matrix.include);
  for (const image of matrix.include) {
    assert.ok(fs.existsSync(path.join(__dirname, "..", "..", image.file)));
  }
  const result = spawnSync(
    process.execPath,
    [path.join(__dirname, "release.cjs"), "image-matrix"],
    {
      cwd: path.join(__dirname, "..", ".."),
      encoding: "utf8",
    },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), matrix);
});

test("matrix rejects ambiguous tags and unsafe artifact identifiers", () => {
  assert.throws(() => validateImageMatrix({ include: [] }), /at least one/);
  assert.throws(() => validateImageMatrix({}), /at least one/);
  for (const override of [
    { id: "../client" },
    { id: "client\nother" },
    { image_suffix: "|injected" },
    { variant: "alpine" },
    { file: "../Dockerfile" },
    { file: "/Dockerfile" },
    { builder_variant: "" },
    { runtime: "" },
    { allow_missing_dev: "true" },
  ]) {
    assert.throws(() =>
      validateImageMatrix({ include: [{ ...matrix.include[0], ...override }] }),
    );
  }
  assert.throws(
    () =>
      validateImageMatrix({ include: [matrix.include[0], matrix.include[0]] }),
    /Duplicate/,
  );
  assert.throws(
    () =>
      validateImageMatrix({
        include: [
          matrix.include[0],
          { ...matrix.include[0], id: "different-id" },
        ],
      }),
    /Duplicate/,
  );
});

function missingImage(reference) {
  return Object.assign(new Error("manifest not found"), {
    status: 1,
    stdout: "",
    stderr: `ERROR: ${reference}: not found\n`,
  });
}

test("stable retries reuse an existing candidate instead of rebuilding it", () => {
  const reference = `ghcr.io/example/aztunnel:stable-v0.4.1-${sha}`;
  const calls = [];
  assert.deepEqual(
    candidateState(reference, (args) => {
      calls.push(args);
      return `Name: ${reference}\nDigest: ${digest}\n`;
    }),
    { build: false, digest },
  );
  assert.deepEqual(calls, [["buildx", "imagetools", "inspect", reference]]);
  assert.deepEqual(
    candidateState(reference, () => {
      throw missingImage(reference);
    }),
    { build: true, digest: "" },
  );
});

test("only explicit manifest absence permits a new stable image build", () => {
  const reference = "ghcr.io/example/aztunnel:absent";
  for (const message of ["manifest unknown", "MANIFEST_UNKNOWN"]) {
    const error = Object.assign(new Error(message), {
      status: 1,
      stderr: message,
    });
    assert.equal(
      inspectImage(reference, () => {
        throw error;
      }),
      null,
    );
  }
  for (const error of [
    Object.assign(new Error("denied"), { status: 1, stderr: "unauthorized" }),
    Object.assign(new Error("registry unavailable"), {
      status: 1,
      stderr: "connection reset",
    }),
    Object.assign(new Error("wrong reference"), {
      status: 1,
      stderr: "ERROR: other: not found",
    }),
    Object.assign(new Error("missing docker"), { code: "ENOENT" }),
  ]) {
    assert.throws(
      () =>
        candidateState(reference, () => {
          throw error;
        }),
      (actual) => actual === error,
    );
  }
  assert.throws(
    () => inspectImage(reference, () => "Digest: invalid"),
    /valid manifest/,
  );
});

function imageRegistry(initial = []) {
  const tags = new Map(initial);
  const writes = [];
  return {
    tags,
    writes,
    execute(args) {
      if (args[2] === "inspect") {
        const reference = args[3];
        if (!tags.has(reference)) throw missingImage(reference);
        return `Name: ${reference}\nDigest: ${tags.get(reference)}\n`;
      }
      assert.equal(args[2], "create");
      const target = args[4];
      const sourceDigest = args[5].split("@")[1];
      writes.push(target);
      tags.set(target, sourceDigest);
      return "";
    },
  };
}

function promotionOptions(images = matrix.include) {
  const imageBase = "ghcr.io/example/aztunnel";
  return {
    version: "v0.4.1",
    imageBase,
    images,
    refs: Object.fromEntries(
      images.map((image) => [
        image.id,
        `${imageBase}${image.image_suffix}@${digest}\n`,
      ]),
    ),
  };
}

function referenceFiles(options) {
  const files = new Map(
    Object.entries(options.refs).map(([id, value]) => [`${id}.txt`, value]),
  );
  return {
    files,
    readdirSync() {
      return [...files.keys()];
    },
    readFileSync(file) {
      const name = path.basename(file);
      assert.ok(files.has(name), name);
      return files.get(name);
    },
  };
}

test("artifact collection and both promotion paths follow added and removed matrix entries", () => {
  const added = {
    ...matrix.include[0],
    id: "client-new-variant",
    variant: "-new-variant",
    allow_missing_dev: true,
  };
  for (const images of [[...matrix.include, added], matrix.include.slice(1)]) {
    const options = promotionOptions(images);
    const refs = readImageRefs(
      options.imageBase,
      images,
      "image-refs",
      referenceFiles(options),
    );
    assert.deepEqual(
      Object.keys(refs),
      images.map(({ id }) => id),
    );
    const entries = developmentImageEntries(options.imageBase, images);
    assert.deepEqual(
      entries,
      images.map(
        ({ id, image_suffix, variant, allow_missing_dev }) =>
          `${options.imageBase}${image_suffix}|dev${variant}|image-refs/${id}.txt|${allow_missing_dev === true}`,
      ),
    );
    const registry = imageRegistry();
    promoteStableImages({ ...options, refs }, registry.execute);
    assert.equal(registry.writes.length, images.length * 3);
    for (const { image_suffix, variant } of images) {
      assert.equal(
        registry.tags.get(
          `${options.imageBase}${image_suffix}:0.4.1${variant}`,
        ),
        digest,
      );
    }
  }
});

test("image artifact validation refuses missing, extra, or mismatched candidates", () => {
  const options = promotionOptions();
  for (const mutation of [
    (files) => files.delete("client-scratch.txt"),
    (files) => files.set("unexpected.txt", options.refs["client-scratch"]),
  ]) {
    const files = referenceFiles(options);
    mutation(files.files);
    assert.throws(
      () =>
        readImageRefs(options.imageBase, options.images, "image-refs", files),
      /do not match/,
    );
  }
  const files = referenceFiles(options);
  files.files.set("client-scratch.txt", `ghcr.io/another/image@${digest}`);
  assert.throws(
    () => readImageRefs(options.imageBase, options.images, "image-refs", files),
    /Invalid candidate/,
  );
});

test("Dependabot merges use the automation credential to trigger main CI", () => {
  const workflow = fs.readFileSync(
    path.join(__dirname, "..", "workflows", "dependabot-auto-merge.yml"),
    "utf8",
  );
  const steps = workflow.replace(/\r\n/g, "\n").split(/^\s+- name: /m);
  const guard = steps.find((step) =>
    step.startsWith("Require automation credential\n"),
  );
  const approval = steps.find((step) => step.startsWith("Approve PR\n"));
  const merge = steps.find((step) => step.startsWith("Enable auto-merge\n"));
  assert.ok(guard);
  assert.ok(approval);
  assert.ok(merge);
  assert.ok(steps.indexOf(guard) < steps.indexOf(approval));
  assert.ok(steps.indexOf(approval) < steps.indexOf(merge));
  assert.match(
    guard,
    /AUTOMATION_TOKEN: \$\{\{ secrets\.GH_AUTOMATION_TOKEN \}\}/,
  );
  assert.match(guard, /if \[\[ -z "\$AUTOMATION_TOKEN" \]\]; then/);
  assert.match(guard, /::error::GH_AUTOMATION_TOKEN is required/);
  assert.match(guard, /exit 1/);
  assert.match(merge, /token: \$\{\{ secrets\.GH_AUTOMATION_TOKEN \}\}/);
  assert.match(merge, /merge-method: squash/);
  assert.doesNotMatch(approval, /GH_AUTOMATION_TOKEN/);
  assert.doesNotMatch(workflow, /uses: actions\/checkout/);
  assert.match(
    workflow,
    /permissions:\r?\n  contents: read\r?\n  pull-requests: write/,
  );
  assert.doesNotMatch(workflow, /contents: write/);
  const condition = (step) =>
    step
      .match(/if: >\n([\s\S]*?)(?=^        \w)/m)?.[1]
      .trim()
      .replace(/\s+/g, " ");
  assert.equal(
    condition(guard),
    [
      "steps.metadata.outputs.update-type == 'version-update:semver-patch' ||",
      "steps.metadata.outputs.update-type == 'version-update:semver-minor' ||",
      "(steps.metadata.outputs.update-type == 'version-update:semver-major' &&",
      "steps.metadata.outputs.package-ecosystem == 'github_actions')",
    ].join(" "),
  );
  assert.equal(condition(guard), condition(approval));
  assert.equal(condition(guard), condition(merge));
});

test("publication workflow collects all image artifacts and generates the dev list", () => {
  const workflow = fs.readFileSync(
    path.join(__dirname, "..", "workflows", "publish-release.yml"),
    "utf8",
  );
  assert.match(workflow, /pattern: image-\*/);
  assert.match(workflow, /merge-multiple: true/);
  assert.match(workflow, /release-images\.cjs validate-refs/);
  assert.match(workflow, /release-images\.cjs dev-images/);
  assert.doesNotMatch(workflow, /name: image-(?:client|relay)-/);
  assert.doesNotMatch(workflow, /"ghcr\.io\/.*\|dev/);
});

test("maintenance workflow gates captured release inputs behind one environment approval", () => {
  const workflows = path.join(__dirname, "..", "workflows");
  const workflow = fs
    .readFileSync(path.join(workflows, "prepare-release.yml"), "utf8")
    .replace(/\r\n/g, "\n");
  const prepareJob = workflow
    .split(/^  prepare:\n/m)[1]
    ?.split(/^  tag:\n/m)[0];
  const tagJob = workflow.split(/^  tag:\n/m)[1];
  assert.ok(prepareJob);
  assert.ok(tagJob);
  assert.match(workflow, /default: patch/);
  assert.ok(prepareJob.includes("RELEASE_BUMP: ${{ inputs.bump || 'patch' }}"));
  assert.doesNotMatch(prepareJob, /^\s+environment:/m);
  assert.match(tagJob, /needs: prepare/);
  assert.match(tagJob, /environment:\n\s+name: stable-release/);
  assert.ok(tagJob.includes("ref: ${{ needs.prepare.outputs.source_sha }}"));
  assert.match(tagJob, /persist-credentials: true/);
  assert.match(tagJob, /await tag\(\{ github, context, core \}\)/);
  for (const [output, input] of [
    ["version", "RELEASE_VERSION"],
    ["source_sha", "RELEASE_SOURCE_SHA"],
    ["previous_tag", "RELEASE_PREVIOUS_TAG"],
  ]) {
    assert.ok(
      prepareJob.includes(`${output}: \${{ steps.plan.outputs.${output} }}`),
    );
    assert.ok(
      tagJob.includes(`${input}: \${{ needs.prepare.outputs.${output} }}`),
    );
  }
  assert.equal(
    fs.existsSync(path.join(workflows, "approve-release.yml")),
    false,
  );
  assert.doesNotMatch(
    workflow,
    /create-pull-request|pull_request:|release\.json|RELEASE_FORCE/,
  );
});

test("stable promotion covers all variants and never rewrites existing exact versions", () => {
  const registry = imageRegistry();
  const options = promotionOptions();
  promoteStableImages(options, registry.execute);
  assert.equal(registry.writes.length, matrix.include.length * 3);
  const exacts = matrix.include.map(
    (image) =>
      `${options.imageBase}${image.image_suffix}:0.4.1${image.variant}`,
  );
  for (const exact of exacts) assert.equal(registry.tags.get(exact), digest);
  registry.writes.length = 0;
  promoteStableImages(options, registry.execute);
  assert.equal(registry.writes.length, matrix.include.length * 2);
  assert.ok(registry.writes.every((version) => !exacts.includes(version)));
  for (const exact of exacts) assert.equal(registry.tags.get(exact), digest);
});

test("any conflicting exact-version digest stops promotion before all tag writes", () => {
  const options = promotionOptions();
  const conflict = `${options.imageBase}-relay:0.4.1-alpine`;
  const registry = imageRegistry([[conflict, `sha256:${"d".repeat(64)}`]]);
  assert.throws(
    () => promoteStableImages(options, registry.execute),
    /Refusing to overwrite immutable/,
  );
  assert.deepEqual(registry.writes, []);
  assert.equal(registry.tags.get(conflict), `sha256:${"d".repeat(64)}`);
});

test("partial promotion retries repair rolling tags without moving an exact version", () => {
  const options = promotionOptions(matrix.include.slice(0, 1));
  const registry = imageRegistry();
  let interrupted = false;
  const execute = (args) => {
    // Simulate the registry accepting an exact tag, then losing the response.
    const result = registry.execute(args);
    if (args[2] === "create" && !interrupted) {
      interrupted = true;
      throw new Error("connection lost after promotion");
    }
    return result;
  };
  assert.throws(() => promoteStableImages(options, execute), /connection lost/);
  const exact = `${options.imageBase}:0.4.1`;
  assert.equal(registry.tags.get(exact), digest);
  promoteStableImages(options, execute);
  assert.equal(
    registry.writes.filter((version) => version === exact).length,
    1,
  );
  assert.equal(registry.tags.get(`${options.imageBase}:0.4`), digest);
  assert.equal(registry.tags.get(`${options.imageBase}:latest`), digest);
});

test("promotion fails closed on invalid references, registry errors and digest mismatches", () => {
  const options = promotionOptions(matrix.include.slice(0, 1));
  assert.throws(
    () =>
      promoteStableImages({
        ...options,
        refs: { "client-scratch": `ghcr.io/other/image@${digest}` },
      }),
    /Invalid candidate reference/,
  );
  assert.throws(
    () =>
      promoteStableImages(options, () => {
        throw new Error("registry unavailable");
      }),
    /registry unavailable/,
  );
  const registry = imageRegistry();
  const execute = (args) => {
    const result = registry.execute(args);
    if (args[2] === "create")
      registry.tags.set(args[4], `sha256:${"d".repeat(64)}`);
    return result;
  };
  assert.throws(
    () => promoteStableImages(options, execute),
    /does not match candidate/,
  );
});

const successfulCI = {
  head_sha: sha,
  head_branch: "main",
  event: "push",
  status: "completed",
  conclusion: "success",
};

function harness({
  releases = [{ tag_name: "v0.4.0" }],
  tags = "v0.4.0",
  runs = [successfulCI],
  sourceSHA = sha,
  tagSHA = sha,
  remoteTags = tags,
  remoteTagSHA = tagSHA,
  remoteMainSHA = oldSHA,
  ancestor = true,
} = {}) {
  const calls = [];
  const outputs = {};
  const refs = new Map([
    ["HEAD", sourceSHA],
    ["origin/main", sourceSHA],
  ]);
  let fetched = false;
  const context = { repo: { owner: "example", repo: "aztunnel" } };
  const github = {
    paginate: async (endpoint, args) => {
      assert.equal(endpoint, github.rest.repos.listReleases);
      assert.deepEqual(args, { ...context.repo, per_page: 100 });
      calls.push({ name: "releases" });
      return releases;
    },
    rest: {
      repos: { listReleases() {} },
      actions: {
        listWorkflowRuns: async (args) => {
          assert.deepEqual(args, {
            ...context.repo,
            workflow_id: "ci.yml",
            branch: "main",
            event: "push",
            head_sha: sourceSHA,
            per_page: 1,
          });
          calls.push({ name: "ci" });
          return { data: { workflow_runs: runs } };
        },
      },
      git: {
        createRef: async (args) => {
          calls.push({ name: "createRef", args });
        },
      },
    },
  };
  const core = {
    summary: {
      addRaw(body) {
        calls.push({ name: "summary", body });
        return this;
      },
      async write() {
        calls.push({ name: "writeSummary" });
      },
    },
    setOutput(key, value) {
      outputs[key] = value;
    },
    notice(message) {
      calls.push({ name: "notice", message });
    },
  };
  const execute = (command, args) => {
    assert.equal(command, "git");
    calls.push({ name: "git", args });
    if (args.join(" ") === "rev-parse HEAD") return sourceSHA;
    if (args[0] === "fetch") {
      assert.deepEqual(args, [
        "fetch",
        "--no-tags",
        "--prune",
        "origin",
        "+refs/heads/main:refs/remotes/origin/main",
        "+refs/tags/v*:refs/tags/v*",
      ]);
      fetched = true;
      refs.set("origin/main", remoteMainSHA);
      return "";
    }
    if (args.join(" ") === "tag --list v*") return fetched ? remoteTags : tags;
    if (args[0] === "merge-base") {
      assert.deepEqual(args, [
        "merge-base",
        "--is-ancestor",
        sourceSHA,
        "origin/main",
      ]);
      assert.equal(fetched, true, "Ancestry must use freshly fetched main.");
      assert.equal(refs.get("origin/main"), remoteMainSHA);
      if (!ancestor) throw new Error("exit 1");
      return "";
    }
    if (args[0] === "rev-parse" && args[1] === "v0.4.1^{commit}")
      return fetched ? remoteTagSHA : tagSHA;
    throw new Error(`Unexpected git arguments: ${args}`);
  };
  return {
    github,
    context,
    core,
    execute,
    outputs,
    calls,
    refs,
    env: {
      RELEASE_VERSION: "v0.4.1",
      RELEASE_SOURCE_SHA: sourceSHA,
      RELEASE_PREVIOUS_TAG: "v0.4.0",
    },
  };
}

test("an unchanged weekly source prepares the next patch and approval summary without release state or a tag", async () => {
  const h = harness();
  await prepare(h);
  assert.deepEqual(h.outputs, {
    version: "v0.4.1",
    source_sha: sha,
    previous_tag: "v0.4.0",
  });
  const summary = h.calls.find(({ name }) => name === "summary").body;
  assert.match(summary, /Proposed maintenance release v0\.4\.1/);
  assert.ok(
    summary.includes(
      `Immutable source: [\`${sha}\`](https://github.com/example/aztunnel/commit/${sha})`,
    ),
  );
  assert.ok(
    summary.includes(
      `https://github.com/example/aztunnel/compare/v0.4.0...${sha}`,
    ),
  );
  assert.equal(h.calls.filter(({ name }) => name === "writeSummary").length, 1);
  assert.deepEqual(
    h.calls.filter(({ name }) => name === "git").map(({ args }) => args),
    [
      ["rev-parse", "HEAD"],
      ["tag", "--list", "v*"],
    ],
  );
  assert.equal(h.calls.filter(({ name }) => name === "createRef").length, 0);
});

test("manual patch, minor and major bumps use the highest published stable release", async () => {
  for (const [bump, version] of [
    ["patch", "v0.10.10"],
    ["minor", "v0.11.0"],
    ["major", "v1.0.0"],
  ]) {
    const h = harness({
      releases: [
        { tag_name: "v0.9.9" },
        { tag_name: "v0.10.9" },
        { tag_name: "v9.0.0", draft: true },
        { tag_name: "v8.0.0", prerelease: true },
        { tag_name: "v10.0.0-rc.1" },
        { tag_name: "dev" },
      ],
      tags: "v0.9.9\r\nv0.10.9\r\nv10.0.0-rc.1",
    });
    h.env.RELEASE_BUMP = bump;
    await prepare(h);
    assert.deepEqual(h.outputs, {
      version,
      source_sha: sha,
      previous_tag: "v0.10.9",
    });
  }
});

test("preparation requires an initial published stable release and rejects malformed baselines", async () => {
  for (const releases of [
    [],
    [{ tag_name: "v1.0.0", draft: true }],
    [{ tag_name: "v1.0.0", prerelease: true }],
  ]) {
    await assert.rejects(
      prepare(harness({ releases })),
      /initial stable release/,
    );
  }
  for (const version of ["v01.2.3", "v9007199254740992.0.0"]) {
    await assert.rejects(
      prepare(harness({ releases: [{ tag_name: version }] })),
      /Invalid stable version/,
    );
  }
});

test("preparation rejects invalid bumps and source SHAs without outputs", async () => {
  for (const bump of ["auto", "", "security", "PATCH"]) {
    const h = harness();
    h.env.RELEASE_BUMP = bump;
    await assert.rejects(prepare(h), /Invalid bump/);
    assert.deepEqual(h.outputs, {});
  }
  for (const sourceSHA of [
    "HEAD",
    "a".repeat(39),
    "g".repeat(40),
    `${sha}\n`,
    "A".repeat(40),
  ]) {
    const h = harness({ sourceSHA });
    await assert.rejects(prepare(h), /Invalid release source SHA/);
    assert.deepEqual(h.outputs, {});
  }
});

test("both stages require the newest exact-source main push CI, never an older success", async () => {
  const cases = [
    [[], /Missing main push CI/],
    [[{ ...successfulCI, head_sha: oldSHA }], /Missing main push CI/],
    [[{ ...successfulCI, head_branch: "feature" }], /Missing main push CI/],
    [[{ ...successfulCI, event: "pull_request" }], /Missing main push CI/],
    ...["queued", "in_progress", "waiting", "requested", "pending"].map(
      (status) => [
        [{ ...successfulCI, status, conclusion: null }, successfulCI],
        /Pending main push CI/,
      ],
    ),
    ...[
      "failure",
      "cancelled",
      "timed_out",
      "skipped",
      "action_required",
      "neutral",
      null,
    ].map((conclusion) => [
      [{ ...successfulCI, conclusion }, successfulCI],
      /Failed main push CI/,
    ]),
  ];
  for (const operation of [prepare, tag]) {
    for (const [runs, message] of cases) {
      const h = harness({ runs });
      await assert.rejects(operation(h), message);
      assert.deepEqual(h.outputs, {});
      assert.equal(
        h.calls.filter(({ name }) => name === "createRef").length,
        0,
      );
    }
  }
});

test("pending stable tags block preparation while nonstable and older tags do not", async () => {
  for (const tags of ["v0.4.0\nv0.4.1", "v0.4.0\nv1.0.0"]) {
    const h = harness({ tags });
    await assert.rejects(prepare(h), /not finished publishing/);
    assert.deepEqual(h.outputs, {});
  }
  await prepare(
    harness({
      tags: "dev\nv0.3.0\nv0.4.0\nv1.0.0-rc.1\nv01.2.3\nv9007199254740992.0.0",
    }),
  );
});

test("tagging refreshes main and tags after API checks but tags only the immutable approved source", async () => {
  const h = harness({ sourceSHA: sha, remoteMainSHA: oldSHA });
  await tag(h);
  assert.deepEqual(
    h.calls.filter(({ name }) => name === "createRef"),
    [
      {
        name: "createRef",
        args: { ...h.context.repo, ref: "refs/tags/v0.4.1", sha },
      },
    ],
  );
  assert.ok(
    h.calls.some(
      ({ name, args }) =>
        name === "git" &&
        args.join(" ") === `merge-base --is-ancestor ${sha} origin/main`,
    ),
  );
  assert.ok(h.calls.some(({ name }) => name === "ci"));
  assert.ok(h.calls.some(({ name }) => name === "releases"));
  assert.equal(h.refs.get("HEAD"), sha);
  assert.equal(h.refs.get("origin/main"), oldSHA);
  assert.deepEqual(
    h.calls
      .filter(({ name }) => name !== "notice")
      .map(({ name, args }) => (name === "git" ? args[0] : name)),
    ["ci", "releases", "fetch", "merge-base", "tag", "createRef"],
  );
  assert.equal(
    h.calls.some(
      ({ name, args }) => name === "git" && args.join(" ") === "rev-parse HEAD",
    ),
    false,
  );
});

test("tagging rejects malformed inputs and versions at or below the approved baseline", async () => {
  for (const env of [
    { RELEASE_VERSION: undefined },
    { RELEASE_VERSION: "v01.4.1" },
    { RELEASE_VERSION: "v9007199254740992.0.0" },
    { RELEASE_PREVIOUS_TAG: undefined },
    { RELEASE_PREVIOUS_TAG: "main" },
    { RELEASE_SOURCE_SHA: undefined },
    { RELEASE_SOURCE_SHA: "HEAD" },
    { RELEASE_SOURCE_SHA: `${sha}\n` },
    { RELEASE_SOURCE_SHA: "g".repeat(40) },
    { RELEASE_VERSION: "v0.4.0" },
    { RELEASE_VERSION: "v0.3.9" },
  ]) {
    const h = harness();
    Object.assign(h.env, env);
    await assert.rejects(tag(h), /Invalid|must be newer/);
    assert.deepEqual(h.calls, []);
  }
});

test("tagging rejects a source no longer reachable from main", async () => {
  const h = harness({ ancestor: false });
  await assert.rejects(tag(h), /not an ancestor of current main/);
  assert.equal(h.calls.filter(({ name }) => name === "createRef").length, 0);
});

test("tagging rejects changed published baselines, including a previously published requested version", async () => {
  for (const version of ["v0.4.1", "v0.5.0", "v0.3.0"]) {
    const h = harness({
      releases: [{ tag_name: version }],
      tags: "v0.4.0\nv0.4.1",
    });
    await assert.rejects(tag(h), /baseline changed/);
    assert.equal(h.calls.filter(({ name }) => name === "createRef").length, 0);
  }
});

test("tagging detects other pending tags pushed after checkout, even below the requested version or during a retry", async () => {
  for (const [version, tags] of [
    ["v0.4.1", "v0.4.0\nv0.4.2"],
    ["v0.5.0", "v0.4.0\nv0.4.1"],
    ["v0.4.1", "v0.4.0\nv0.4.1\nv0.5.0"],
  ]) {
    const h = harness({ tags: "v0.4.0", remoteTags: tags });
    h.env.RELEASE_VERSION = version;
    await assert.rejects(tag(h), /not finished publishing/);
    assert.equal(h.calls.filter(({ name }) => name === "createRef").length, 0);
  }
});

test("same-tag retries discover remote tags pushed after checkout; conflicting sources fail", async () => {
  const h = harness({ tags: "v0.4.0", remoteTags: "v0.4.0\nv0.4.1" });
  await tag(h);
  assert.equal(h.calls.filter(({ name }) => name === "createRef").length, 0);
  assert.match(
    h.calls.find(({ name }) => name === "notice").message,
    /Follow or rerun its Stable Release.*not be moved or recreated/,
  );
  const conflict = harness({
    tags: "v0.4.0",
    remoteTags: "v0.4.0\nv0.4.1",
    remoteTagSHA: oldSHA,
  });
  await assert.rejects(tag(conflict), /different source/);
  assert.equal(
    conflict.calls.filter(({ name }) => name === "createRef").length,
    0,
  );
});

test("a failed remote refresh stops tagging rather than trusting checkout refs", async () => {
  const h = harness();
  const execute = h.execute;
  h.execute = (command, args) => {
    if (args[0] === "fetch") throw new Error("Fetch authentication failed");
    return execute(command, args);
  };
  await assert.rejects(tag(h), /Fetch authentication failed/);
  assert.equal(
    h.calls.some(({ name }) => name === "createRef"),
    false,
  );
});

test("real git refresh detects a stable tag pushed after the approved source checkout", async (t) => {
  const directory = fs.mkdtempSync(path.join(process.cwd(), ".release-git-"));
  t.after(() =>
    fs.rmSync(directory, { recursive: true, force: true, maxRetries: 3 }),
  );
  const remote = path.join(directory, "origin.git");
  const seed = path.join(directory, "seed");
  const checkout = path.join(directory, "checkout");
  const git = (cwd, args) =>
    execFileSync(
      "git",
      [
        "-c",
        "user.name=Release Test",
        "-c",
        "user.email=release-test@example.invalid",
        "-c",
        "commit.gpgsign=false",
        "-c",
        "tag.gpgsign=false",
        ...args,
      ],
      {
        cwd,
        encoding: "utf8",
        stdio: ["ignore", "pipe", "pipe"],
        env: {
          ...process.env,
          GIT_CONFIG_NOSYSTEM: "1",
          GIT_CONFIG_GLOBAL: path.join(directory, "no-global-config"),
          GIT_TERMINAL_PROMPT: "0",
        },
      },
    ).trim();
  git(directory, ["init", "--bare", remote]);
  git(directory, ["init", "--initial-branch=main", seed]);
  git(seed, ["commit", "--allow-empty", "-m", "Initial published source"]);
  const sourceSHA = git(seed, ["rev-parse", "HEAD"]);
  git(seed, ["tag", "v0.4.0"]);
  git(seed, ["remote", "add", "origin", remote]);
  git(seed, ["push", "origin", "main", "refs/tags/v0.4.0"]);
  git(directory, ["clone", "--branch", "main", remote, checkout]);
  git(checkout, ["checkout", "--detach", sourceSHA]);
  assert.equal(git(checkout, ["tag", "--list", "v0.4.2"]), "");

  git(seed, ["commit", "--allow-empty", "-m", "Main advanced after checkout"]);
  const mainSHA = git(seed, ["rev-parse", "HEAD"]);
  git(seed, ["tag", "v0.4.2"]);
  git(seed, ["push", "origin", "main", "refs/tags/v0.4.2"]);
  assert.equal(git(checkout, ["rev-parse", "origin/main"]), sourceSHA);
  assert.equal(git(checkout, ["tag", "--list", "v0.4.2"]), "");

  const h = harness({
    sourceSHA,
    runs: [{ ...successfulCI, head_sha: sourceSHA }],
  });
  h.execute = (command, args) => {
    assert.equal(command, "git");
    return git(checkout, args);
  };
  await assert.rejects(tag(h), /v0\.4\.2 have not finished publishing/);
  assert.equal(git(checkout, ["rev-parse", "HEAD"]), sourceSHA);
  assert.equal(git(checkout, ["rev-parse", "origin/main"]), mainSHA);
  assert.equal(git(checkout, ["rev-parse", "v0.4.2^{commit}"]), mainSHA);
  assert.equal(
    h.calls.some(({ name }) => name === "createRef"),
    false,
  );
});

test("API and createRef failures propagate without fallback or mutation retries", async () => {
  for (const operation of [prepare, tag]) {
    for (const api of ["releases", "ci"]) {
      const h = harness();
      const fail = async () => {
        throw new Error("GitHub unavailable");
      };
      if (api === "releases") h.github.paginate = fail;
      else h.github.rest.actions.listWorkflowRuns = fail;
      await assert.rejects(operation(h), /GitHub unavailable/);
      assert.equal(
        h.calls.filter(({ name }) => name === "createRef").length,
        0,
      );
    }
  }
  const h = harness();
  let attempts = 0;
  h.github.rest.git.createRef = async () => {
    attempts++;
    throw new Error("Reference already exists");
  };
  await assert.rejects(tag(h), /Reference already exists/);
  assert.equal(attempts, 1);
});
