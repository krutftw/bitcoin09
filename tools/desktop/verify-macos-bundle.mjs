import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { verifyWalletEditionBinary } from "./verify-wallet-edition.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(scriptDirectory, "..", "..");

export function bundlePaths(appPath) {
  const contents = path.join(appPath, "Contents");
  return {
    infoPlist: path.join(contents, "Info.plist"),
    shell: path.join(contents, "MacOS", "btc09-wallet"),
    sidecar: path.join(contents, "MacOS", "btc09-core"),
  };
}

export function assertNoBuildPathLeaks(bytes, additionalRoots = []) {
  const fixedRoots = [
    "C:\\Users\\appveyor",
    "C:\\projects\\bitcoin09",
    "/Users/appveyor",
    "/Users/cirrus",
    "/Users/runner/work/bitcoin09",
  ];
  const candidates = new Set();
  for (const value of [...fixedRoots, ...additionalRoots]) {
    if (!value) continue;
    candidates.add(value);
    candidates.add(value.replaceAll("\\", "/"));
    candidates.add(value.replaceAll("/", "\\"));
  }
  for (const candidate of candidates) {
    if (candidate && bytes.includes(Buffer.from(candidate))) {
      throw new Error(`macOS bundle contains a local or CI build path: ${candidate}`);
    }
  }
}

function checked(command, args) {
  const result = spawnSync(command, args, {
    cwd: root,
    encoding: "utf8",
    windowsHide: true,
  });
  if (result.error || result.status !== 0) {
    const detail = `${result.stdout || ""}${result.stderr || ""}`.trim();
    throw new Error(detail || result.error?.message || `${command} failed.`);
  }
  return `${result.stdout || ""}${result.stderr || ""}`.trim();
}

function plistValue(infoPlist, key) {
  return checked("plutil", ["-extract", key, "raw", "-o", "-", infoPlist]);
}

export function verifyMacOSBundle(appPath, expectedVersion) {
  if (process.platform !== "darwin") {
    throw new Error("The macOS application bundle must be verified on macOS.");
  }
  const resolved = path.resolve(appPath);
  const paths = bundlePaths(resolved);
  for (const [name, file] of Object.entries(paths)) {
    if (!existsSync(file)) throw new Error(`macOS bundle is missing ${name}: ${file}`);
  }

  const packageVersion = JSON.parse(
    readFileSync(path.join(root, "walletapp", "package.json"), "utf8"),
  ).version;
  const version = expectedVersion || packageVersion;
  if (plistValue(paths.infoPlist, "CFBundleShortVersionString") !== version) {
    throw new Error(`macOS bundle version does not match ${version}.`);
  }
  if (plistValue(paths.infoPlist, "CFBundleIdentifier") !== "org.bitcoin09.wallet") {
    throw new Error("macOS bundle identifier is not org.bitcoin09.wallet.");
  }

  for (const binary of [paths.shell, paths.sidecar]) {
    checked("lipo", ["-verify_arch", "arm64", "x86_64", binary]);
    assertNoBuildPathLeaks(readFileSync(binary), [process.cwd(), process.env.CIRRUS_WORKING_DIR]);
  }

  checked("codesign", ["--verify", "--deep", "--strict", "--verbose=2", resolved]);
  const signature = checked("codesign", ["-d", "--verbose=4", resolved]);
  if (!/Signature=adhoc/i.test(signature) || !/TeamIdentifier=not set/i.test(signature)) {
    throw new Error("macOS CI bundle was expected to use an ad-hoc, non-distribution signature.");
  }

  const thinDirectory = mkdtempSync(path.join(tmpdir(), "btc09-macos-sidecar-"));
  try {
    for (const architecture of ["arm64", "x86_64"]) {
      const thin = path.join(thinDirectory, `btc09-core-${architecture}`);
      checked("lipo", [paths.sidecar, "-thin", architecture, "-output", thin]);
      verifyWalletEditionBinary(thin);
    }
  } finally {
    rmSync(thinDirectory, { recursive: true, force: true });
  }

  return resolved;
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  if (process.argv.length < 3 || process.argv.length > 4) {
    throw new Error("Usage: node verify-macos-bundle.mjs PATH_TO_APP [EXPECTED_VERSION]");
  }
  const verified = verifyMacOSBundle(process.argv[2], process.argv[3]);
  process.stdout.write(`Verified universal wallet-only macOS app: ${verified}\n`);
}
