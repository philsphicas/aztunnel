const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");
const {
  approve,
  classify,
  compareVersions,
  describe,
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
  validateRequest,
} = require("./release.cjs");

const sha = "a".repeat(40);
const oldSHA = "b".repeat(40);
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

test("shared image matrix retains every published variant", () => {
  assert.deepEqual(
    matrix.include.map((image) => image.id),
    [
      "client-scratch",
      "client-alpine",
      "client-bookworm",
      "client-azurelinux3",
      "relay-bookworm",
      "relay-alpine",
    ],
  );
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
  assert.deepEqual(commits, [
    commit("fix: a bug\n\nDetails", ["README.md", "internal/arc/arc.go"]),
  ]);
});

test("a reserved unpublished version blocks new preparation", () => {
  assert.throws(
    () => requireNoPendingTag("v0.4.0", () => "dev\nv0.4.0\nv0.4.1"),
    /not finished/,
  );
  requireNoPendingTag("v0.4.0", () => "dev\nv0.4.0\nv0.5.0-rc.1");
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
  commits = [],
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
      repos: { listReleases() {} },
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
    open: [{ number: 12 }],
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
  ancestry.execute = () => {
    throw new Error("not an ancestor");
  };
  await assert.rejects(approve(ancestry), /not an ancestor/);
  for (const h of [stale, conflict, ci, ancestry]) {
    assert.ok(!h.calls.some((call) => call.name === "createRef"));
  }
});
