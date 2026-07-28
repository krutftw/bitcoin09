import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  statSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const blockedContentMarkers = [
  "navigator.webdriver",
  "demoScreen",
  "demoCall",
  "demoPhrase",
  "?demo=",
  "anchor begin canvas daring elder fabric",
  "4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn",
  "/api/v1/miner/",
  "Start mining",
  "BTC09 miner help report",
];

const signingExtensions = new Set([".jks", ".keystore", ".p12", ".pfx"]);

function jarCommand() {
  const executable = process.platform === "win32" ? "jar.exe" : "jar";
  if (process.env.JAVA_HOME) {
    const configured = path.join(process.env.JAVA_HOME, "bin", executable);
    if (existsSync(configured)) return configured;
  }
  const settings = spawnSync("java", ["-XshowSettings:properties", "-version"], {
    encoding: "utf8",
    windowsHide: true,
  });
  const output = `${settings.stdout || ""}\n${settings.stderr || ""}`;
  const javaHome = output.match(/^\s*java\.home\s*=\s*(.+)\s*$/m)?.[1]?.trim();
  if (javaHome) {
    const discovered = path.join(javaHome, "bin", executable);
    if (existsSync(discovered)) return discovered;
  }
  return "jar";
}

export function resolveAndroidSdkRoot(options = {}) {
  const platform = options.platform || process.platform;
  const env = options.env || process.env;
  const directoryExists = options.directoryExists || existsSync;
  const paths = platform === "win32" ? path.win32 : path;
  const candidates = [
    env.ANDROID_HOME,
    env.ANDROID_SDK_ROOT,
    platform === "win32" && env.LOCALAPPDATA
      ? paths.join(env.LOCALAPPDATA, "Android", "Sdk")
      : undefined,
    platform !== "win32" ? paths.join(os.homedir(), "Android", "Sdk") : undefined,
  ].filter(Boolean);
  return candidates.find((candidate) => directoryExists(candidate));
}

function apksignerCommand() {
  const executable = process.platform === "win32" ? "apksigner.bat" : "apksigner";
  const sdkRoot = resolveAndroidSdkRoot();
  if (!sdkRoot) return executable;
  const buildToolsRoot = path.join(sdkRoot, "build-tools");
  if (!existsSync(buildToolsRoot)) return executable;
  const versions = readdirSync(buildToolsRoot, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort((left, right) => right.localeCompare(left, undefined, { numeric: true }));
  for (const version of versions) {
    const candidate = path.join(buildToolsRoot, version, executable);
    if (existsSync(candidate)) return candidate;
  }
  return executable;
}

function javaCommand() {
  const executable = process.platform === "win32" ? "java.exe" : "java";
  if (process.env.JAVA_HOME) {
    const configured = path.join(process.env.JAVA_HOME, "bin", executable);
    if (existsSync(configured)) return configured;
  }
  return executable;
}

export function resolveApksignerInvocation(command, options = {}) {
  const platform = options.platform || process.platform;
  if (platform !== "win32" || !command.toLowerCase().endsWith(".bat")) {
    return { command, args: [] };
  }
  const paths = path.win32;
  const fileExists = options.fileExists || existsSync;
  const signerJar = paths.join(paths.dirname(command), "lib", "apksigner.jar");
  if (!paths.isAbsolute(command) || !fileExists(signerJar)) {
    throw new Error("Android signature verification needs ANDROID_HOME or ANDROID_SDK_ROOT to locate apksigner.jar.");
  }
  return {
    command: options.javaExecutable || javaCommand(),
    args: ["-jar", signerJar],
  };
}

export function assertAndroidApkSignature({ status, stdout, stderr }) {
  const output = `${stdout || ""}\n${stderr || ""}`;
  if (status !== 0 || !/Signer #1 certificate SHA-256 digest:/i.test(output)) {
    throw new Error("The Android APK is not signed by a valid release certificate.");
  }
  return output.match(/Signer #1 certificate SHA-256 digest:\s*([^\r\n]+)/i)?.[1]?.trim();
}

export function verifyAndroidApkSignature(artifactPath) {
  const resolved = path.resolve(artifactPath);
  if (path.extname(resolved).toLowerCase() !== ".apk") {
    throw new Error("Android signature verification requires an APK file.");
  }
  const invocation = resolveApksignerInvocation(apksignerCommand());
  const result = spawnSync(invocation.command, [...invocation.args, "verify", "--verbose", "--print-certs", resolved], {
    encoding: "utf8",
    windowsHide: true,
    maxBuffer: 16 * 1024 * 1024,
  });
  if (result.error) {
    throw new Error(`Android signature verification could not start: ${result.error.message}`);
  }
  return assertAndroidApkSignature(result);
}

export function assertMobileArtifactEntries(entries) {
  if (!Array.isArray(entries) || entries.length === 0 || entries.length > 50_000) {
    throw new Error("The mobile artifact has an invalid file count.");
  }
  for (const entry of entries) {
    const name = String(entry?.name || "").replaceAll("\\", "/");
    if (!name || signingExtensions.has(path.extname(name).toLowerCase())) {
      throw new Error("The mobile artifact contains signing material or an invalid entry.");
    }
    const bytes = Buffer.isBuffer(entry.bytes) ? entry.bytes : Buffer.from(entry.bytes || "");
    const text = bytes.toString("latin1");
    for (const marker of blockedContentMarkers) {
      if (text.includes(marker)) {
        throw new Error("The mobile artifact contains forbidden demo or mining content.");
      }
    }
  }
}

function collectFiles(root) {
  const files = [];
  const pending = [root];
  while (pending.length > 0) {
    const current = pending.pop();
    for (const item of readdirSync(current, { withFileTypes: true })) {
      const resolved = path.join(current, item.name);
      if (item.isDirectory()) {
        pending.push(resolved);
      } else if (item.isFile()) {
        const details = statSync(resolved);
        if (details.size > 256 * 1024 * 1024) {
          throw new Error("The mobile artifact contains an unexpectedly large file.");
        }
        files.push({ name: path.relative(root, resolved), bytes: readFileSync(resolved) });
      }
      if (files.length + pending.length > 50_000) {
        throw new Error("The mobile artifact has too many entries.");
      }
    }
  }
  return files;
}

export function verifyMobileArtifact(artifactPath) {
  const resolved = path.resolve(artifactPath);
  if (!new Set([".apk", ".aab"]).has(path.extname(resolved).toLowerCase())) {
    throw new Error("Only Android APK and AAB artifacts can be verified.");
  }
  const temporaryRoot = realpathSync(os.tmpdir());
  const extractionRoot = mkdtempSync(path.join(temporaryRoot, "btc09-mobile-artifact-"));
  const cleanupPrefix = temporaryRoot.endsWith(path.sep) ? temporaryRoot : temporaryRoot + path.sep;
  if (!extractionRoot.startsWith(cleanupPrefix)) {
    throw new Error("The mobile artifact temporary path is unsafe.");
  }
  try {
    const extraction = spawnSync(jarCommand(), ["xf", resolved], {
      cwd: extractionRoot,
      encoding: "utf8",
      windowsHide: true,
      maxBuffer: 16 * 1024 * 1024,
    });
    if (extraction.error || extraction.status !== 0) {
      throw new Error(extraction.error?.message || extraction.stderr?.trim() || "The mobile artifact could not be unpacked.");
    }
    assertMobileArtifactEntries(collectFiles(extractionRoot));
    return resolved;
  } finally {
    rmSync(extractionRoot, { recursive: true, force: true });
  }
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  if (process.argv.length < 3) {
    throw new Error("Usage: node verify-artifact.mjs ARTIFACT [ARTIFACT...]");
  }
  for (const artifact of process.argv.slice(2)) {
    process.stdout.write(`Verified mobile wallet artifact: ${verifyMobileArtifact(artifact)}\n`);
    if (process.env.BTC09_REQUIRE_ANDROID_SIGNATURE === "1" && path.extname(artifact).toLowerCase() === ".apk") {
      const fingerprint = verifyAndroidApkSignature(artifact);
      process.stdout.write(`Verified Android release signer: ${fingerprint}\n`);
    }
  }
}
