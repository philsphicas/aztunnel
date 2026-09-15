const assert = require("node:assert/strict");
const { execFileSync, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");
const {
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
const requestCommitSHA = "e".repeat(40);
const digest = `sha256:${"c".repeat(64)}`;
const inputs = {
  images: { "golang:1.27.1-bookworm": digest },
  azurelinux_packages: {
    "linux/amd64": ["openssl-libs-3.0"],
    "linux/arm64": ["openssl-libs-3.0"],
  },
};
const commit = (message, files = ["go.mod"]) => ({ sha, message, files });
const makePlan = (options = {}) =>
  plan({
    previous: "v0.4.0",
    sourceSHA: sha,
    commits: [],
    inputs,
    baseline: inputs,
    ...options,
  });

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
    "dev",
  ];
  const script = path.join(__dirname, "release.cjs");
  for (const tag of [...valid, ...invalid]) {
    const result = spawnSync(
      process.execPath,
      [script, "validate-version", tag],
      { encoding: "utf8" },
    );
    assert.ifError(result.error);
    assert.equal(result.status === 0, valid.includes(tag), tag);
    assert.equal(isStableVersion(tag), valid.includes(tag), tag);
    if (!valid.includes(tag))
      assert.match(result.stderr, /Invalid stable version/);
  }
  assert.equal(latestVersion(["v0.9.0", "v0.10.0", ...invalid]), "v0.10.0");
  const result = spawnSync(process.execPath, [script, "latest-version"], {
    input: ["v0.9.0", "v0.10.0", ...invalid, ""].join("\r\n"),
    encoding: "utf8",
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), "v0.10.0");
  assert.throws(() => latestVersion(invalid), /No valid stable version/);
  assert.throws(
    () => nextVersion("v0.0.9007199254740991", "patch"),
    /Invalid stable version/,
  );
});

test("shipping filter excludes docs, test code and automation-only changes", () => {
  for (const file of [
    "README.md",
    "docs/releases.md",
    "internal/arc/arc_test.go",
    "internal/relay/testdata/input.json",
    "e2e/scenarios/basic.go",
    "e2e/infra/main.go",
    ".github/workflows/ci.yml",
    ".github/scripts/release.cjs",
    ".github/release.json",
    ".github/dependabot.yml",
    "scripts/update-go-version.sh",
    ".golangci-version",
    ".golangci.yml",
    "mockrelay/.golangci.yml",
    "mockrelay/.golangci.yaml",
    "mockrelay/README.md",
    "mockrelay/relay_test.go",
  ])
    assert.equal(ships(file), false, file);
  for (const file of [
    "go.mod",
    "go.sum",
    "go.work",
    "go.work.sum",
    "e2e/go.mod",
    "e2e/go.sum",
    "e2e/infra/go.mod",
    "e2e/infra/go.sum",
    "Dockerfile",
    "Dockerfile.azurelinux3",
    ".dockerignore",
    "Makefile",
    "LICENSE",
    "cmd/aztunnel/main.go",
    "internal/arc/arc.go",
    "mockrelay/go.mod",
    "mockrelay/Dockerfile",
    ".github/release-images.json",
    ".github/workflows/publish-release.yml",
    ".github/actions/package-release/action.yml",
  ])
    assert.equal(ships(file), true, file);
});

test("unchanged and non-shipping-only weeks produce no release", () => {
  assert.equal(makePlan(), null);
  assert.equal(
    makePlan({
      commits: [commit("test: update scenarios", ["e2e/scenarios/basic.go"])],
    }),
    null,
  );
  assert.equal(
    makePlan({ commits: [commit("docs: update instructions", ["README.md"])] }),
    null,
  );
});

test("lint-only module configuration never triggers or changes a release decision", () => {
  const lint = commit("chore: adjust lint rules", ["mockrelay/.golangci.yml"]);
  assert.equal(makePlan({ commits: [lint] }), null);
  const request = makePlan({
    commits: [
      lint,
      commit("fix: repair relay behavior", ["mockrelay/relay.go"]),
    ],
  });
  assert.equal(request.version, "v0.4.1");
  assert.equal(request.decision_required, false);
  assert.equal(request.reasons.length, 1);
});

test("workspace-only dependency updates are never silently skipped", () => {
  for (const module of ["e2e", "e2e/infra"]) {
    const commits = [
      commit("chore(deps): fix shared dependency CVE", [
        `${module}/go.mod`,
        `${module}/go.sum`,
      ]),
    ];
    const request = makePlan({ commits });
    assert.equal(request.version, "v0.4.1");
    assert.equal(request.decision_required, false);
    validateRequest(request, "v0.4.0", commits);
  }
});

test("maintenance shipping changes automatically propose a patch", () => {
  for (const message of [
    "chore(deps): bump golang.org/x/sync",
    "chore(go): update Go to 1.27.1",
    "fix(arc): reconnect reliably",
    "perf: reduce allocations",
    "revert: broken update",
  ]) {
    const request = makePlan({ commits: [commit(message)] });
    assert.equal(request.version, "v0.4.1");
    assert.equal(request.decision_required, false);
    validateRequest(request, "v0.4.0", [commit(message)]);
  }
});

test("features and ambiguous changes require a deliberate version decision", () => {
  for (const message of [
    "feat: add a listener mode",
    "Add Azure Linux 3 container variant (#162)",
  ]) {
    const commits = [commit(message)];
    const request = makePlan({ commits });
    assert.equal(request.version, "v0.5.0");
    assert.equal(request.decision_required, true);
    assert.throws(
      () => validateRequest(request, "v0.4.0", commits),
      /explicit version/,
    );
    const decided = makePlan({ commits, bump: "minor" });
    assert.equal(decided.decision_required, false);
    validateRequest(decided, "v0.4.0", commits);
  }
  assert.throws(
    () => makePlan({ commits: [commit("feat: new mode")], bump: "patch" }),
    /at least a minor/,
  );
  assert.equal(
    makePlan({ commits: [commit("Repair a bug")], bump: "patch" }).version,
    "v0.4.1",
  );
});

test("breaking declarations take precedence regardless of commit order", () => {
  for (const message of [
    "fix!: remove an option",
    "fix: update\n\nBREAKING CHANGE: remove an option",
    "feat(api)!: new interface",
  ]) {
    const commits = [
      commit(message),
      commit("feat: add another option"),
      commit("fix: bug"),
    ];
    assert.equal(classify(commits, "v1.2.3").minimum, "major");
    assert.equal(classify([...commits].reverse(), "v1.2.3").minimum, "major");
    assert.equal(makePlan({ commits }).version, "v0.5.0");
    assert.equal(makePlan({ commits, previous: "v1.2.3" }).version, "v2.0.0");
    assert.throws(
      () => makePlan({ commits, previous: "v1.2.3", bump: "minor" }),
      /at least a major/,
    );
  }
});

test("container changes and a forced refresh can release an unchanged commit", () => {
  assert.equal(makePlan({ baseline: null }).version, "v0.4.1");
  assert.equal(makePlan({ force: true }).version, "v0.4.1");
  assert.equal(makePlan({ force: true }).force, true);
  const changed = structuredClone(inputs);
  changed.azurelinux_packages["linux/arm64"] = ["openssl-libs-3.1"];
  const request = makePlan({ inputs: changed });
  assert.equal(request.version, "v0.4.1");
  assert.equal(request.source_sha, sha);
  assert.match(request.reasons[0], /OS packages changed/);
  const reordered = {
    azurelinux_packages: inputs.azurelinux_packages,
    images: inputs.images,
  };
  assert.equal(makePlan({ inputs: reordered }), null);
});

test("approval rejects stale, malformed and artificially readied requests", () => {
  const request = makePlan({ force: true });
  assert.throws(() => validateRequest(request, "v0.4.1", []), /stale/);
  for (const override of [
    { source_sha: "--help" },
    { version: "v0.4.99" },
    { decision_required: true },
    { explicit_bump: undefined },
    { container_inputs: {} },
    { container_inputs: { images: { golang: "invalid" } } },
  ])
    assert.throws(() =>
      validateRequest({ ...request, ...override }, "v0.4.0", []),
    );
  const commits = [commit("Add an unclassified feature")];
  const draft = makePlan({ commits });
  assert.throws(
    () =>
      validateRequest(
        { ...draft, decision_required: false },
        "v0.4.0",
        commits,
      ),
    /explicit version/,
  );
});

const matrix = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "release-images.json"), "utf8"),
);
const rpmInventory = "ca-certificates-1.0.noarch\nopenssl-libs-3.0.aarch64\n";
function dockerOutput(command, args) {
  assert.equal(command, "docker");
  return args[0] === "run" ? rpmInventory : `Name: base\nDigest: ${digest}\n`;
}

test("shared image matrix validates its definitions and runtime package probe", () => {
  assert.equal(validateImageMatrix(matrix), matrix.include);
  for (const image of matrix.include) {
    assert.ok(fs.existsSync(path.join(__dirname, "..", "..", image.file)));
  }
  const dockerfile = fs.readFileSync(
    path.join(__dirname, "..", "..", "Dockerfile.azurelinux3"),
    "utf8",
  );
  for (const command of [
    "tdnf upgrade -y",
    "tdnf install -y openssl-libs ca-certificates",
  ]) {
    assert.ok(dockerfile.includes(command));
    assert.ok(packageProbe.includes(command));
  }
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

test("Dependabot explicitly emits the routine release prefix for every ecosystem", () => {
  const config = fs.readFileSync(
    path.join(__dirname, "..", "dependabot.yml"),
    "utf8",
  );
  const ecosystems = config.match(/^\s*- package-ecosystem:/gm);
  const prefixes = config.match(/^\s+prefix: "?chore\(deps\)"?\s*$/gm);
  assert.ok(ecosystems.length > 0);
  assert.equal(prefixes.length, ecosystems.length);
});

test("observations deduplicate bases, pin probes and inspect both architectures", () => {
  const calls = [];
  const observed = observeInputs(
    matrix,
    "toolchain go1.27.1\r\n",
    (command, args) => {
      calls.push(args);
      return dockerOutput(command, args);
    },
  );
  assert.equal(Object.keys(observed.images).length, 6);
  assert.deepEqual(Object.keys(observed.azurelinux_packages), [
    "linux/amd64",
    "linux/arm64",
  ]);
  const inspections = calls.filter((args) => args[0] === "buildx");
  assert.equal(inspections.length, 6);
  assert.ok(
    inspections.some(
      (args) =>
        args.at(-1) ===
        "mcr.microsoft.com/oss/go/microsoft/golang:1.27.1-azurelinux3.0",
    ),
  );
  assert.ok(inspections.every((args) => args.at(-1) !== "scratch"));
  for (const args of calls.filter((args) => args[0] === "run")) {
    assert.ok(
      args.includes(`mcr.microsoft.com/azurelinux/base/core:3.0@${digest}`),
    );
    assert.equal(args.at(-1), packageProbe);
  }
});

test("registry and package probe failures never look like an unchanged week", () => {
  assert.throws(() => observeInputs(matrix, "go 1.27.0"), /toolchain/);
  assert.throws(
    () => observeInputs(matrix, "toolchain go1.27.1", () => "Digest: invalid"),
    /valid manifest/,
  );
  assert.throws(
    () =>
      observeInputs(matrix, "toolchain go1.27.1", () => {
        throw new Error("registry unavailable");
      }),
    /registry unavailable/,
  );
  assert.throws(
    () =>
      observeInputs(matrix, "toolchain go1.27.1", (command, args) =>
        args[0] === "run" ? "" : dockerOutput(command, args),
      ),
    /Incomplete/,
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
    {
      build: true,
      digest: "",
    },
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
  const condition = (step) => step.match(/if: >\n([\s\S]*?)(?=^        \w)/m)?.[1];
  assert.ok(condition(guard));
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
  assert.ok(registry.writes.every((tag) => !exacts.includes(tag)));
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
  assert.equal(registry.writes.filter((tag) => tag === exact).length, 1);
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

test("commit inspection uses ancestry and complete per-commit shipping paths", () => {
  const calls = [];
  const commits = readCommits("v0.4.0", sha, (command, args) => {
    calls.push(args);
    if (args[0] === "rev-list") return sha;
    if (args[0] === "show") return "fix: a bug\n\nDetails";
    if (args[0] === "diff") return "README.md\0internal/arc/arc.go\0";
    return "";
  });
  assert.deepEqual(calls[0], ["merge-base", "--is-ancestor", "v0.4.0", sha]);
  assert.ok(calls.find((args) => args[0] === "diff").includes("--no-renames"));
  assert.deepEqual(commits, [
    commit("fix: a bug\n\nDetails", ["README.md", "internal/arc/arc.go"]),
  ]);
});

test("moving shipped code into tests still triggers a release with rename detection enabled", (t) => {
  const directory = fs.mkdtempSync(
    path.join(os.tmpdir(), "aztunnel-release-renames-"),
  );
  t.after(() =>
    fs.rmSync(directory, { recursive: true, force: true, maxRetries: 3 }),
  );
  const globalConfig = path.join(directory, ".gitconfig");
  fs.writeFileSync(globalConfig, "");
  const execute = (command, args) =>
    execFileSync(
      command,
      [
        "-c",
        "user.name=Release regression",
        "-c",
        "user.email=release-test@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      {
        cwd: directory,
        encoding: "utf8",
        env: {
          ...process.env,
          GIT_CONFIG_NOSYSTEM: "1",
          GIT_CONFIG_GLOBAL: globalConfig,
        },
      },
    ).trim();
  execute("git", ["init", "--quiet", "--initial-branch=main"]);
  execute("git", ["config", "diff.renames", "true"]);
  fs.mkdirSync(path.join(directory, "internal"));
  fs.mkdirSync(path.join(directory, "e2e"));
  fs.writeFileSync(
    path.join(directory, "internal", "helper.go"),
    "package helper\n",
  );
  execute("git", ["add", "--", "internal/helper.go"]);
  execute("git", ["commit", "--quiet", "-m", "Initial shipped helper"]);
  execute("git", ["tag", "v0.4.0"]);
  execute("git", ["mv", "--", "internal/helper.go", "e2e/helper.go"]);
  execute("git", [
    "commit",
    "--quiet",
    "-m",
    "refactor: move helper into tests",
  ]);
  const sourceSHA = execute("git", ["rev-parse", "HEAD"]);

  const destination = execute("git", [
    "diff",
    "--name-only",
    "-M",
    "v0.4.0",
    sourceSHA,
  ]);
  assert.equal(destination, "e2e/helper.go");
  assert.equal(
    makePlan({
      sourceSHA,
      commits: [
        {
          sha: sourceSHA,
          message: "refactor: move helper into tests",
          files: [destination],
        },
      ],
    }),
    null,
  );

  const commits = readCommits("v0.4.0", sourceSHA, execute);
  assert.deepEqual(commits[0].files, ["e2e/helper.go", "internal/helper.go"]);
  const request = makePlan({ sourceSHA, commits });
  assert.ok(request);
  assert.equal(request.decision_required, true);
});

test("a reserved unpublished version blocks new preparation", () => {
  assert.throws(
    () => requireNoPendingTag("v0.4.0", () => "dev\nv0.4.0\nv0.4.1"),
    /not finished/,
  );
  requireNoPendingTag("v0.4.0", () => "dev\nv0.4.0\nv0.5.0-rc.1");
  requireNoPendingTag("v0.4.0", () => "v0.4.0\nv01.2.3\nv9007199254740992.0.0");
});

test("CI gate requires the newest run for the exact source to succeed", async () => {
  for (const latest of [
    undefined,
    { head_sha: oldSHA, conclusion: "success" },
    { head_sha: sha, conclusion: null },
    { head_sha: sha, conclusion: "failure" },
  ]) {
    const github = {
      rest: {
        actions: {
          listWorkflowRuns: async () => ({
            data: { workflow_runs: latest ? [latest] : [] },
          }),
        },
      },
    };
    await assert.rejects(requireCI(github, {}, sha), /must succeed/);
  }
});

function harness({
  request = null,
  versions = ["v0.4.0"],
  tags = "v0.4.0",
  open = [],
  pending = null,
  commits = [],
  requestCommit = requestCommitSHA,
  provenance = [
    {
      merged_at: "2026-09-11T00:00:00Z",
      base: { ref: "main" },
      head: {
        ref: "automation/maintenance-release",
        repo: { full_name: "example/aztunnel" },
      },
    },
  ],
} = {}) {
  const disk = new Map([
    [".github/release-images.json", JSON.stringify(matrix)],
    ["go.work", "toolchain go1.27.1"],
  ]);
  if (request) disk.set(".github/release.json", JSON.stringify(request));
  const calls = [];
  const outputs = {};
  const context = { repo: { owner: "example", repo: "aztunnel" } };
  const record = (name) => async (args) => {
    calls.push({ name, args });
    return { data: {} };
  };
  const github = {
    paginate: async () => versions.map((tag_name) => ({ tag_name })),
    rest: {
      repos: {
        listReleases() {},
        listPullRequestsAssociatedWithCommit: async (args) => {
          calls.push({ name: "requestProvenance", args });
          return { data: provenance };
        },
        getContent: async () => {
          assert.ok(pending, "An open release PR needs a request fixture.");
          return {
            data: {
              content: Buffer.from(JSON.stringify(pending)).toString("base64"),
            },
          };
        },
      },
      actions: {
        listWorkflowRuns: async () => ({
          data: { workflow_runs: [{ head_sha: sha, conclusion: "success" }] },
        }),
      },
      pulls: { list: async () => ({ data: open }), update: record("updatePR") },
      issues: { createComment: record("comment") },
      git: { createRef: record("createRef") },
    },
  };
  const summary = {
    addRaw(body) {
      calls.push({ name: "summary", body });
      return this;
    },
    async write() {},
  };
  const core = {
    setOutput(key, value) {
      outputs[key] = value;
    },
    summary,
    notice: (message) => calls.push({ name: "notice", message }),
  };
  const files = {
    existsSync: (file) => disk.has(file),
    readFileSync: (file) => {
      assert.ok(disk.has(file), file);
      return disk.get(file);
    },
    writeFileSync: (file, content) => disk.set(file, content),
  };
  const execute = (command, args) => {
    if (command === "docker") return dockerOutput(command, args);
    assert.equal(command, "git");
    if (args[0] === "tag") return tags;
    if (args[0] === "merge-base") return "";
    if (args[0] === "log") {
      calls.push({ name: "requestOrigin", args });
      return requestCommit;
    }
    if (args[0] === "rev-parse")
      return args[1] === "v0.4.0^{commit}" ? oldSHA : sha;
    if (args[0] === "rev-list")
      return commits.map((item) => item.sha).join("\n");
    if (args[0] === "show")
      return commits.find((item) => item.sha === args.at(-1)).message;
    if (args[0] === "diff")
      return commits.find((item) => item.sha === args.at(-1)).files.join("\0");
    throw new Error(`Unexpected git arguments: ${args}`);
  };
  return {
    github,
    context,
    core,
    files,
    execute,
    disk,
    outputs,
    calls,
    env: { RUNNER_TEMP: "temporary" },
  };
}

test("preparation creates a durable request and approval notes but no tag", async () => {
  const h = harness({ commits: [commit("chore(deps): update")] });
  await prepare(h);
  const request = JSON.parse(h.disk.get(".github/release.json"));
  assert.equal(request.version, "v0.4.1");
  assert.equal(request.source_sha, sha);
  assert.equal(h.outputs.ready, true);
  assert.match(h.disk.get(h.outputs.body_path), /Merge this PR to publish/);
  assert.ok(!h.calls.some((call) => call.name === "createRef"));
  assert.match(describe(request, "example/aztunnel"), new RegExp(sha));
});

test("unchanged preparation writes no files and closes a superseded candidate", async () => {
  const baseline = observeInputs(matrix, "toolchain go1.27.1", dockerOutput);
  const h = harness({
    request: {
      version: "v0.4.0",
      source_sha: oldSHA,
      container_inputs: baseline,
    },
    open: [{ number: 12, head: { sha: oldSHA } }],
    pending: { previous_tag: "v0.3.0", source_sha: oldSHA },
  });
  h.env.RELEASE_BUMP = "patch";
  const before = new Map(h.disk);
  await prepare(h);
  assert.equal(h.outputs.ready, false);
  assert.deepEqual(h.disk, before);
  assert.ok(
    h.calls.some(
      (call) => call.name === "updatePR" && call.args.state === "closed",
    ),
  );
});

test("preparation waits for an approved request that has not created its tag yet", async () => {
  const h = harness({ request: makePlan({ force: true }) });
  await assert.rejects(prepare(h), /awaiting publication/);
  assert.ok(!h.calls.some((call) => call.name === "createRef"));
});

test("unsupported previously published stable versions fail instead of being skipped", async () => {
  for (const tag of ["v01.2.3", "v9007199254740992.0.0"]) {
    for (const versions of [[tag], ["v0.4.0", tag]]) {
      const h = harness({ versions });
      await assert.rejects(prepare(h), /Invalid stable version/);
      assert.ok(!h.disk.has(".github/release.json"));
    }
  }
});

test("weekly refresh preserves an explicit decision for the same source", async () => {
  const commits = [commit("feat: new mode")];
  const pending = makePlan({ commits, bump: "minor" });
  const h = harness({ commits, open: [{ number: 12, head: { sha: oldSHA } }] });
  h.github.rest.repos.getContent = async () => ({
    data: { content: Buffer.from(JSON.stringify(pending)).toString("base64") },
  });
  await prepare(h);
  assert.equal(h.outputs.version, "v0.5.0");
  assert.equal(h.outputs.decision_required, false);
  pending.source_sha = oldSHA;
  await prepare({ ...h, files: { ...h.files, existsSync: () => false } });
  assert.equal(h.outputs.decision_required, true);
});

test("forced-only requests survive weekly refreshes and explicit bump dispatches", async () => {
  const baseline = observeInputs(matrix, "toolchain go1.27.1", dockerOutput);
  let pending = makePlan({ inputs: baseline, baseline, force: true });
  for (const bump of ["auto", "auto", "patch"]) {
    const h = harness({
      request: {
        version: "v0.4.0",
        source_sha: oldSHA,
        container_inputs: baseline,
      },
      open: [{ number: 12, head: { sha: oldSHA } }],
      pending,
    });
    h.env.RELEASE_BUMP = bump;
    h.env.RELEASE_FORCE = "false";
    await prepare(h);
    pending = JSON.parse(h.disk.get(".github/release.json"));
    assert.equal(h.outputs.ready, true);
    assert.equal(pending.force, true);
    assert.equal(pending.version, "v0.4.1");
    assert.ok(!h.calls.some(({ name }) => name === "updatePR"));
  }
});

test("force intent never leaks to a different baseline or source", async () => {
  const baseline = observeInputs(matrix, "toolchain go1.27.1", dockerOutput);
  const pending = makePlan({ inputs: baseline, baseline, force: true });
  for (const override of [
    { previous_tag: "v0.3.0" },
    { source_sha: oldSHA },
    { force: false },
  ]) {
    const h = harness({
      request: {
        version: "v0.4.0",
        source_sha: oldSHA,
        container_inputs: baseline,
      },
      open: [{ number: 12, head: { sha: oldSHA } }],
      pending: { ...pending, ...override },
    });
    await prepare(h);
    assert.equal(h.outputs.ready, false);
    assert.ok(
      h.calls.some(
        ({ name, args }) => name === "updatePR" && args.state === "closed",
      ),
    );
  }
});

test("approval tags only the approved source, not the request's merge commit", async () => {
  const request = makePlan({ force: true });
  const h = harness({ request });
  await approve(h);
  assert.deepEqual(h.calls.find((call) => call.name === "createRef").args, {
    owner: "example",
    repo: "aztunnel",
    ref: "refs/tags/v0.4.1",
    sha,
  });
});

test("approval requires a merged release PR from this repository", async () => {
  const valid = {
    merged_at: "2026-09-11T00:00:00Z",
    base: { ref: "main" },
    head: {
      ref: "automation/maintenance-release",
      repo: { full_name: "example/aztunnel" },
    },
  };
  for (const provenance of [
    [],
    [{ ...valid, merged_at: null }],
    [{ ...valid, base: { ref: "release/0.4" } }],
    [{ ...valid, head: { ...valid.head, ref: "some-other-feature" } }],
    [
      {
        ...valid,
        head: { ...valid.head, repo: { full_name: "fork/aztunnel" } },
      },
    ],
  ]) {
    const h = harness({ request: makePlan({ force: true }), provenance });
    await assert.rejects(approve(h), /did not arrive through a merged/);
    assert.ok(!h.calls.some(({ name }) => name === "createRef"));
  }
});

test("approval finds request provenance independently of HEAD and merge method", async () => {
  const provenance = [
    {
      merged_at: "2026-09-11T00:00:00Z",
      base: { ref: "main" },
      head: { ref: "unrelated" },
    },
    {
      merged_at: "2026-09-11T00:00:00Z",
      merge_commit_sha: oldSHA,
      base: { ref: "main" },
      head: {
        ref: "automation/maintenance-release",
        repo: { full_name: "example/aztunnel" },
      },
    },
  ];
  const h = harness({ request: makePlan({ force: true }), provenance });
  h.context.sha = "f".repeat(40);
  await approve(h);
  assert.deepEqual(h.calls.find(({ name }) => name === "requestOrigin").args, [
    "log",
    "--first-parent",
    "-1",
    "--format=%H",
    "--",
    ".github/release.json",
  ]);
  assert.equal(
    h.calls.find(({ name }) => name === "requestProvenance").args.commit_sha,
    requestCommitSHA,
  );
  assert.equal(h.calls.find(({ name }) => name === "createRef").args.sha, sha);
});

test("approval of an absent or deleted request is an explicit no-op", async () => {
  const h = harness();
  await approve(h);
  assert.deepEqual(h.calls, [
    {
      name: "notice",
      message: "No release request on main; nothing to approve.",
    },
  ]);
});

test("missing request history and failed provenance lookups never create a tag", async () => {
  const missing = harness({
    request: makePlan({ force: true }),
    requestCommit: "",
  });
  await assert.rejects(approve(missing), /No mainline commit/);
  const unavailable = harness({ request: makePlan({ force: true }) });
  unavailable.github.rest.repos.listPullRequestsAssociatedWithCommit =
    async () => {
      throw new Error("GitHub unavailable");
    };
  await assert.rejects(approve(unavailable), /GitHub unavailable/);
  for (const h of [missing, unavailable]) {
    assert.ok(!h.calls.some(({ name }) => name === "createRef"));
  }
});

test("approval reruns never overwrite or recreate an existing tag", async () => {
  for (const versions of [["v0.4.0"], ["v0.4.0", "v0.4.1"]]) {
    const h = harness({
      request: makePlan({ force: true }),
      versions,
      tags: "v0.4.0\nv0.4.1",
    });
    await approve(h);
    assert.ok(!h.calls.some((call) => call.name === "createRef"));
    assert.ok(h.calls.some((call) => call.name === "notice"));
  }
});

test("approval rejects stale requests, conflicting tags, failed CI and missing ancestry", async () => {
  const request = makePlan({ force: true });
  const stale = harness({ request, versions: ["v0.5.0"] });
  await assert.rejects(approve(stale), /stale/);
  const conflict = harness({ request, tags: "v0.4.0\nv0.4.1" });
  const execute = conflict.execute;
  conflict.execute = (command, args) =>
    args[0] === "rev-parse" && args[1] === "v0.4.1^{commit}"
      ? oldSHA
      : execute(command, args);
  await assert.rejects(approve(conflict), /different source/);
  const ci = harness({ request });
  ci.github.rest.actions.listWorkflowRuns = async () => ({
    data: { workflow_runs: [] },
  });
  await assert.rejects(approve(ci), /must succeed/);
  const ancestry = harness({ request });
  const ancestryExecute = ancestry.execute;
  ancestry.execute = (command, args) => {
    if (args[0] === "merge-base") throw new Error("not an ancestor");
    return ancestryExecute(command, args);
  };
  await assert.rejects(approve(ancestry), /not an ancestor/);
  for (const h of [stale, conflict, ci, ancestry]) {
    assert.ok(!h.calls.some((call) => call.name === "createRef"));
  }
});
