const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { parseVersion } = require("./release.cjs");

const digestPattern = /^sha256:[0-9a-f]{64}$/;

function docker(args) {
  return execFileSync("docker", args, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

function inspectImage(reference, execute = docker) {
  let output;
  try {
    output = execute(["buildx", "imagetools", "inspect", reference]);
  } catch (error) {
    const details = `${error.stdout || ""}\n${error.stderr || ""}`;
    if (
      Number.isInteger(error.status) &&
      error.status !== 0 &&
      (/manifest unknown|MANIFEST_UNKNOWN/i.test(details) ||
        details
          .split(/\r?\n/)
          .some((line) => line === `ERROR: ${reference}: not found`))
    ) {
      return null;
    }
    throw error;
  }
  const digest = /^Digest:\s+(sha256:[0-9a-f]{64})\s*$/m.exec(output)?.[1];
  if (!digest) throw new Error(`No valid manifest digest for ${reference}.`);
  return digest;
}

function candidateState(reference, execute = docker) {
  const digest = inspectImage(reference, execute);
  return { build: digest === null, digest: digest || "" };
}

function promoteStableImages(
  { version, imageBase, images, refs },
  execute = docker,
) {
  parseVersion(version);
  const semver = version.slice(1);
  const minor = semver.slice(0, semver.lastIndexOf("."));
  // Preflight every exact-version tag before touching any published tag.
  const promotions = images.map(({ id, image_suffix, variant }) => {
    const image = `${imageBase}${image_suffix}`;
    const source = refs[id]?.trim();
    const digest = source?.slice(image.length + 1);
    if (!source?.startsWith(`${image}@`) || !digestPattern.test(digest)) {
      throw new Error(`Invalid candidate reference for ${id}.`);
    }
    const exact = `${image}:${semver}${variant}`;
    const existing = inspectImage(exact, execute);
    if (existing !== null && existing !== digest) {
      throw new Error(
        `Refusing to overwrite immutable ${exact}: existing ${existing}, candidate ${digest}.`,
      );
    }
    return {
      source,
      digest,
      exact,
      existing,
      rolling: [`${image}:${minor}${variant}`, `${image}:latest${variant}`],
    };
  });
  for (const { source, digest, exact, existing, rolling } of promotions) {
    // A retry skips an already-promoted exact version but repairs rolling tags.
    const targets = existing === null ? [exact, ...rolling] : rolling;
    for (const target of targets) {
      execute(["buildx", "imagetools", "create", "--tag", target, source]);
      if (inspectImage(target, execute) !== digest) {
        throw new Error(
          `Promoted ${target} does not match candidate ${digest}.`,
        );
      }
    }
  }
}

module.exports = { candidateState, inspectImage, promoteStableImages };

if (require.main === module) {
  switch (process.argv[2]) {
    case "candidate": {
      const state = candidateState(process.argv[3]);
      console.log(`build=${state.build}\ndigest=${state.digest}`);
      break;
    }
    case "promote": {
      const images = JSON.parse(
        fs.readFileSync(".github/release-images.json", "utf8"),
      ).include;
      const refs = Object.fromEntries(
        images.map(({ id }) => [
          id,
          fs.readFileSync(path.join(process.argv[5], `${id}.txt`), "utf8"),
        ]),
      );
      promoteStableImages({
        version: process.argv[3],
        imageBase: process.argv[4],
        images,
        refs,
      });
      break;
    }
    default:
      throw new Error(
        "Usage: release-images.cjs candidate <reference> | promote <version> <image-base> <refs-directory>",
      );
  }
}
