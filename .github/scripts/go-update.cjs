const fs = require("node:fs");
const { validateImageMatrix } = require("./release.cjs");
const { inspectImage } = require("./release-images.cjs");

// Every release image must be buildable on both published platforms.
const requiredPlatforms = ["linux/amd64", "linux/arm64"];
const goVersionPattern = /^(?:go)?1\.(\d+)\.(\d+)$/;
const upstreamRepository = "golang";

function parseGoVersion(version) {
  const match = goVersionPattern.exec(version);
  if (!match) {
    throw new Error(`Expected an exact stable Go version, got '${version}'.`);
  }
  return { minor: Number(match[1]), patch: Number(match[2]) };
}

function normalizeGoVersion(version) {
  const { minor, patch } = parseGoVersion(version);
  return `1.${minor}.${patch}`;
}

function compareGoVersions(left, right) {
  const a = parseGoVersion(left);
  const b = parseGoVersion(right);
  return a.minor - b.minor || a.patch - b.patch;
}

function languageVersion(version) {
  return `1.${parseGoVersion(version).minor}`;
}

// Upstream builders gate the update because required CI checks build with them.
// Other distributions (Microsoft Go) may lag: the update still lands and only
// their non-required container builds fail until the image is published.
function builderReferences(images, version) {
  validateImageMatrix({ include: images });
  const target = normalizeGoVersion(version);
  const builders = new Map();
  for (const { builder_repository, builder_variant } of images) {
    const repository = builder_repository || upstreamRepository;
    const reference = `${repository}:${target}-${builder_variant}`;
    builders.set(reference, {
      reference,
      required: repository === upstreamRepository,
    });
  }
  return [...builders.values()];
}

function sleepSync(milliseconds) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds);
}

function inspectBuilder(reference, execute) {
  const digest = inspectImage(reference, execute);
  if (digest === null) {
    return { reference, ready: false, reason: "not published" };
  }
  const index = JSON.parse(
    execute(["buildx", "imagetools", "inspect", "--raw", reference]),
  );
  const platforms = new Set(
    (index.manifests || [])
      .filter(({ platform }) => platform)
      .map(({ platform }) => `${platform.os}/${platform.architecture}`),
  );
  const missing = requiredPlatforms.filter(
    (platform) => !platforms.has(platform),
  );
  if (missing.length > 0) {
    return { reference, ready: false, reason: `missing ${missing.join(", ")}` };
  }
  return { reference, ready: true, digest };
}

// Only a confirmed absence counts as "not ready"; registry or network failures
// are retried and then surfaced, never mistaken for publication lag.
function probeBuilder(
  reference,
  { execute, attempts = 3, sleep = sleepSync } = {},
) {
  let lastError;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return inspectBuilder(reference, execute);
    } catch (error) {
      lastError = error;
      if (attempt < attempts) sleep(5000 * attempt);
    }
  }
  throw new Error(
    `Could not inspect ${reference} after ${attempts} attempts: ${lastError.stderr || lastError.message}`,
  );
}

function selectGoUpdate({ current, releases, lintGo, probe }) {
  const base = parseGoVersion(current);
  const lintLanguage = parseGoVersion(lintGo).minor;
  const candidates = [
    ...new Set(
      releases
        .filter(
          ({ stable, version }) => stable && goVersionPattern.test(version),
        )
        .map(({ version }) => normalizeGoVersion(version)),
    ),
  ]
    .filter((version) => compareGoVersions(version, current) > 0)
    .filter((version) => {
      const { minor } = parseGoVersion(version);
      return minor === base.minor || minor === base.minor + 1;
    })
    .sort((left, right) => compareGoVersions(right, left));

  const deferred = [];
  for (const version of candidates) {
    if (parseGoVersion(version).minor > lintLanguage) {
      deferred.push({
        version,
        reason: `golangci-lint is built with go${normalizeGoVersion(lintGo)}, older than Go ${languageVersion(version)}`,
      });
      continue;
    }
    const builders = probe(version);
    const blocking = builders.filter(
      ({ ready, required }) => required && !ready,
    );
    if (blocking.length === 0) {
      return {
        status: "update",
        target: version,
        requiresReview: languageVersion(version) !== languageVersion(current),
        builders,
        deferred,
      };
    }
    deferred.push({
      version,
      reason: blocking
        .map(({ reference, reason }) => `${reference}: ${reason}`)
        .join("; "),
    });
  }
  return {
    status: candidates.length === 0 ? "current" : "deferred",
    target: normalizeGoVersion(current),
    requiresReview: false,
    builders: [],
    deferred,
  };
}

function builderReport({ builders }) {
  return builders
    .map(({ reference, ready, digest, reason }) =>
      ready
        ? `- \`${reference}\`: \`${digest}\``
        : `- \`${reference}\`: **${reason}**; its container build fails until it is published`,
    )
    .join("\n");
}

function deferralReport({ deferred }) {
  return deferred
    .map(({ version, reason }) => `Go ${version} deferred: ${reason}`)
    .join("\n");
}

module.exports = {
  builderReferences,
  builderReport,
  compareGoVersions,
  deferralReport,
  probeBuilder,
  selectGoUpdate,
};

if (require.main === module) {
  if (process.argv[2] !== "select" || process.argv.length !== 7) {
    throw new Error(
      "Usage: go-update.cjs select <current-go> <golangci-lint-build-go> <releases.json> <report-directory>",
    );
  }
  const [current, lintGo, releasesFile, reportDirectory] =
    process.argv.slice(3);
  const { docker } = require("./release-images.cjs");
  const images = JSON.parse(
    fs.readFileSync(".github/release-images.json", "utf8"),
  ).include;
  const selection = selectGoUpdate({
    current,
    lintGo,
    releases: JSON.parse(fs.readFileSync(releasesFile, "utf8")),
    probe: (version) =>
      builderReferences(images, version).map((builder) => ({
        ...builder,
        ...probeBuilder(builder.reference, { execute: docker }),
      })),
  });
  fs.writeFileSync(
    `${reportDirectory}/builders.md`,
    `${builderReport(selection)}\n`,
  );
  fs.writeFileSync(
    `${reportDirectory}/deferred.txt`,
    `${deferralReport(selection)}\n`,
  );
  console.log(
    [
      `status=${selection.status}`,
      `target-go=${selection.target}`,
      `requires-review=${selection.requiresReview}`,
    ].join("\n"),
  );
}
