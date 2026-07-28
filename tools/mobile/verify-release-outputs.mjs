import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  verifyAndroidApkSignature,
  verifyMobileArtifact,
} from "./verify-artifact.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

export function selectReleaseApk(directory, fileExists = existsSync) {
  const signed = path.join(directory, "app-universal-release.apk");
  const unsigned = path.join(directory, "app-universal-release-unsigned.apk");
  const existing = [signed, unsigned].filter((candidate) => fileExists(candidate));
  if (existing.length !== 1) {
    throw new Error(
      "Android verification requires exactly one fresh signed or unsigned release APK.",
    );
  }
  return {
    path: existing[0],
    signed: existing[0] === signed,
  };
}

const apkDirectory = path.join(
  root,
  "walletapp",
  "src-tauri",
  "gen",
  "android",
  "app",
  "build",
  "outputs",
  "apk",
  "universal",
  "release",
);
const aab = path.join(
  root,
  "walletapp",
  "src-tauri",
  "gen",
  "android",
  "app",
  "build",
  "outputs",
  "bundle",
  "universalRelease",
  "app-universal-release.aab",
);

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  const selected = selectReleaseApk(apkDirectory);
  verifyMobileArtifact(selected.path);
  verifyMobileArtifact(aab);
  process.stdout.write(`Verified Android release APK: ${selected.path}\n`);
  process.stdout.write(`Verified Android release bundle: ${aab}\n`);
  if (selected.signed) {
    const fingerprint = verifyAndroidApkSignature(selected.path);
    process.stdout.write(`Verified Android release signer: ${fingerprint}\n`);
  } else {
    process.stdout.write("Verified unsigned Android CI preflight boundary.\n");
  }
}
