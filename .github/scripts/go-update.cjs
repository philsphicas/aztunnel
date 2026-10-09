const fs = require("node:fs");
const { validateImageMatrix } = require("./release.cjs");
const { inspectImage } = require("./release-images.cjs");

// Every release image must be buildable on both published platforms.
const requiredPlatforms = ["linux/amd64", "linux/arm64"];
const goVersionPattern = /^(?:go)?1\.(\d+)\.(\d+)$/;

function parseGoVersion(version) {
  const match = goVersionPattern.exec(version);
  if (!match) throw new Error(`Expected an exact stable Go version, got '${version}'.`);
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

function builderReferences(images, version) {
  validateImageMatrix({ include: images });
  const target = normalizeGoVersion(version);
  return [
    ...new Set(
      images.map(
        ({ builder_repository, builder_variant }) =>
          `${builder_repository || "golang"}:${target}-${builder_variant}`,
      ),
    ),
  ];
}

function sleepSync(milliseconds) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds);
}

function inspectBuilder(reference, execute) {
  const digest = inspectImage(reference, execute);
  if (digest === null) return { reference, ready: false, reason: "not published" };
  const index = JSON.parse(
    execute(["buildx", "imagetools", "inspect", "--raw", reference]),
  );
  const platforms = new Set(
    (index.manifests || [])
      .filter(({ platform }) => platform)
      .map(({ platform }) => `${platform.os}/${platform.architecture}`),
  );
  const missing = requiredPlatforms.filter((platform) => !platforms.has(platform));
  if (missing.length > 0) {
    return { reference, ready: false, reason: `missing ${missing.join(", ")}` };
  }
  return { reference, ready: true, digest };
}

// Only a confirmed absence counts as "not ready"; registry or network failures
// are retried and then surfaced, never mistaken for publication lag.
function probeBuilder(reference, { execute, attempts = 3, sleep = sleepSync } = {}) {
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

function securityRelease(history, version) {
  const id = `go${normalizeGoVersion(version)}`;
  const start = history.indexOf(`<p id="${id}">`);
  if (start === -1) return false;
  const end = history.indexOf("</p>", start);
  const text = history
    .slice(start, end === -1 ? undefined : end)
    .replace(/<[^>]*>/g, " ")
    .replace(/\s+/g, " ");
  return /\bincludes security fixes\b/.test(text);
}

function selectGoUpdate({ current, releases, lintGo, probe, history = "" }) {
  const base = parseGoVersion(current);
  const lintLanguage = parseGoVersion(lintGo).minor;
  const candidates = [
    ...new Set(
      releases
        .filter(({ stable, version }) => stable && goVersionPattern.test(version))
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
    const security = securityRelease(history, version);
    if (parseGoVersion(version).minor > lintLanguage) {
      deferred.push({
        version,
        security,
        reason: `golangci-lint is built with go${normalizeGoVersion(lintGo)}, older than Go ${languageVersion(version)}`,
      });
      continue;
    }
    const builders = probe(version);
    const missing = builders.filter(({ ready }) => !ready);
    if (missing.length === 0) {
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
      security,
      reason: missing.map(({ reference, reason }) => `${reference}: ${reason}`).join("; "),
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

function deferralReport(selection) {
  return selection.deferred
    .map(
      ({ version, security, reason }) =>
        `- Go ${version}${security ? " (**security release**)" : ""}: ${reason}`,
    )
    .join("\n");
}

function builderReport(selection) {
  return selection.builders
    .map(({ reference, digest }) => `- \`${reference}\` → \`${digest}\``)
    .join("\n");
}

module.exports = {
  builderReferences,
  compareGoVersions,
  deferralReport,
  probeBuilder,
  securityRelease,
  selectGoUpdate,
};

if (require.main === module) {
  if (process.argv[2] !== "select" || process.argv.length !== 8) {
    throw new Error(
      "Usage: go-update.cjs select <current-go> <golangci-lint-build-go> <releases.json> <release-history.html> <report-directory>",
    );
  }
  const [current, lintGo, releasesFile, historyFile, reportDirectory] = process.argv.slice(3);
  const { docker } = require("./release-images.cjs");
  const images = JSON.parse(fs.readFileSync(".github/release-images.json", "utf8")).include;
  const selection = selectGoUpdate({
    current,
    lintGo,
    releases: JSON.parse(fs.readFileSync(releasesFile, "utf8")),
    history: fs.readFileSync(historyFile, "utf8"),
    probe: (version) =>
      builderReferences(images, version).map((reference) =>
        probeBuilder(reference, { execute: docker }),
      ),
  });
  fs.writeFileSync(`${reportDirectory}/builders.md`, `${builderReport(selection)}\n`);
  fs.writeFileSync(`${reportDirectory}/deferred.md`, `${deferralReport(selection)}\n`);
  console.log(
    [
      `status=${selection.status}`,
      `target-go=${selection.target}`,
      `requires-review=${selection.requiresReview}`,
      `deferred=${selection.deferred.length > 0}`,
      `security-deferred=${selection.deferred.some(({ security }) => security)}`,
    ].join("\n"),
  );
}
