const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");
const {
  builderReferences,
  builderReport,
  compareGoVersions,
  deferralReport,
  probeBuilder,
  selectGoUpdate,
} = require("./go-update.cjs");

// Built indirectly so check-go-version.sh does not treat test data as builder pins.
const upstream = "golang";
const microsoft = "mcr.microsoft.com/oss/go/microsoft/golang";
const digest = `sha256:${"d".repeat(64)}`;
const matrix = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "release-images.json"), "utf8"),
);

function missingError(reference) {
  return Object.assign(new Error("not found"), {
    status: 1,
    stderr: `ERROR: ${reference}: not found`,
  });
}

function registry(images) {
  const calls = [];
  const execute = (args) => {
    calls.push(args);
    const reference = args.at(-1);
    const image = images[reference];
    if (image === undefined) throw missingError(reference);
    if (image instanceof Error) throw image;
    if (args.includes("--raw")) {
      return JSON.stringify({
        manifests: image.map((platform) => {
          const [os, architecture] = platform.split("/");
          return { platform: { os, architecture } };
        }),
      });
    }
    return `Name: ${reference}\nDigest: ${digest}`;
  };
  return { calls, execute };
}

const multiArch = ["linux/amd64", "linux/arm64", "unknown/unknown"];
const noSleep = () => {};

function releases(...versions) {
  return versions.map((version) => ({ version: `go${version}`, stable: true }));
}

function probeWith(images) {
  const { execute } = registry(images);
  return (version) =>
    builderReferences(matrix.include, version).map((builder) => ({
      ...builder,
      ...probeBuilder(builder.reference, { execute, sleep: noSleep }),
    }));
}

function allBuilders(version) {
  return Object.fromEntries(
    builderReferences(matrix.include, version).map(({ reference }) => [
      reference,
      multiArch,
    ]),
  );
}

test("builder references mirror the release image matrix", () => {
  assert.deepEqual(builderReferences(matrix.include, "go1.27.2"), [
    { reference: `${upstream}:1.27.2-bookworm`, required: true },
    { reference: `${upstream}:1.27.2-alpine`, required: true },
    { reference: `${microsoft}:1.27.2-azurelinux3.0`, required: false },
  ]);
});

test("Go versions compare numerically", () => {
  assert.ok(compareGoVersions("1.27.10", "1.27.9") > 0);
  assert.ok(compareGoVersions("go1.28.0", "1.27.12") > 0);
  assert.equal(compareGoVersions("go1.27.2", "1.27.2"), 0);
  assert.throws(() => compareGoVersions("1.28rc1", "1.27.0"), /exact stable/);
});

test("probe distinguishes ready, unpublished, and single-platform builders", () => {
  const ready = `${upstream}:1.27.2-bookworm`;
  const amd64Only = `${upstream}:1.27.2-alpine`;
  const { execute } = registry({
    [ready]: multiArch,
    [amd64Only]: ["linux/amd64"],
  });
  assert.deepEqual(probeBuilder(ready, { execute, sleep: noSleep }), {
    reference: ready,
    ready: true,
    digest,
  });
  assert.deepEqual(probeBuilder(amd64Only, { execute, sleep: noSleep }), {
    reference: amd64Only,
    ready: false,
    reason: "missing linux/arm64",
  });
  const absent = `${microsoft}:1.27.2-azurelinux3.0`;
  assert.deepEqual(probeBuilder(absent, { execute, sleep: noSleep }), {
    reference: absent,
    ready: false,
    reason: "not published",
  });
});

test("probe retries transient registry errors and then fails loudly", () => {
  const reference = `${upstream}:1.27.2-bookworm`;
  const outage = Object.assign(new Error("rate limited"), {
    status: 1,
    stderr: "ERROR: toomanyrequests: rate limit exceeded",
  });
  const { calls, execute } = registry({ [reference]: outage });
  const sleeps = [];
  assert.throws(
    () => probeBuilder(reference, { execute, sleep: (ms) => sleeps.push(ms) }),
    /after 3 attempts: ERROR: toomanyrequests/,
  );
  assert.equal(calls.length, 3);
  assert.deepEqual(sleeps, [5000, 10000]);

  let failures = 1;
  const flaky = (args) => {
    if (failures-- > 0) throw outage;
    return registry({ [reference]: multiArch }).execute(args);
  };
  assert.equal(
    probeBuilder(reference, { execute: flaky, sleep: noSleep }).ready,
    true,
  );
});

test("selects the highest builder-ready patch and defers newer unready ones", () => {
  const images = allBuilders("1.27.2");
  images[`${upstream}:1.27.3-bookworm`] = multiArch;
  const selection = selectGoUpdate({
    current: "1.27.1",
    lintGo: "go1.27.0",
    releases: releases("1.27.3", "1.27.2", "1.27.1", "1.26.9"),
    probe: probeWith(images),
  });
  assert.equal(selection.status, "update");
  assert.equal(selection.target, "1.27.2");
  assert.equal(selection.requiresReview, false);
  assert.equal(selection.builders.length, 3);
  assert.deepEqual(
    selection.deferred.map(({ version }) => version),
    ["1.27.3"],
  );
  assert.match(selection.deferred[0].reason, /alpine: not published/);
});

test("a lagging Microsoft builder does not hold back the update", () => {
  const images = allBuilders("1.27.2");
  delete images[`${microsoft}:1.27.2-azurelinux3.0`];
  const selection = selectGoUpdate({
    current: "1.27.1",
    lintGo: "go1.27.0",
    releases: releases("1.27.2", "1.27.1"),
    probe: probeWith(images),
  });
  assert.equal(selection.status, "update");
  assert.equal(selection.target, "1.27.2");
  assert.deepEqual(selection.deferred, []);
  assert.equal(
    builderReport(selection),
    [
      `- \`${upstream}:1.27.2-bookworm\`: \`${digest}\``,
      `- \`${upstream}:1.27.2-alpine\`: \`${digest}\``,
      `- \`${microsoft}:1.27.2-azurelinux3.0\`: **not published**; its container build fails until it is published`,
    ].join("\n"),
  );
});

test("defers when an upstream builder is missing", () => {
  const images = allBuilders("1.27.2");
  images[`${upstream}:1.27.2-alpine`] = ["linux/amd64"];
  const selection = selectGoUpdate({
    current: "1.27.1",
    lintGo: "go1.27.0",
    releases: releases("1.27.2", "1.27.1"),
    probe: probeWith(images),
  });
  assert.equal(selection.status, "deferred");
  assert.equal(selection.target, "1.27.1");
  assert.equal(
    deferralReport(selection),
    `Go 1.27.2 deferred: ${upstream}:1.27.2-alpine: missing linux/arm64`,
  );
});

test("holds a Go feature update until golangci-lint is built with it", () => {
  const images = { ...allBuilders("1.28.0"), ...allBuilders("1.27.3") };
  const options = {
    current: "1.27.2",
    releases: releases("1.28.0", "1.27.3", "1.27.2"),
    probe: probeWith(images),
  };
  const held = selectGoUpdate({ ...options, lintGo: "go1.27.3" });
  assert.equal(held.target, "1.27.3");
  assert.equal(held.requiresReview, false);
  assert.match(
    held.deferred[0].reason,
    /golangci-lint is built with go1\.27\.3/,
  );

  const allowed = selectGoUpdate({ ...options, lintGo: "go1.28.0" });
  assert.equal(allowed.target, "1.28.0");
  assert.equal(allowed.requiresReview, true);
  assert.deepEqual(allowed.deferred, []);
});

test("ignores unstable, older, and too-distant releases", () => {
  const probed = [];
  const selection = selectGoUpdate({
    current: "1.27.2",
    lintGo: "go1.29.0",
    releases: [
      ...releases("1.29.0", "1.27.2", "1.27.1", "1.26.9"),
      { version: "go1.28rc1", stable: false },
    ],
    probe: (version) => {
      probed.push(version);
      return [];
    },
  });
  assert.deepEqual(probed, []);
  assert.equal(selection.status, "current");
  assert.deepEqual(selection.deferred, []);
});
