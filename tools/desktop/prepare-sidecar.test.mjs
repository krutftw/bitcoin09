import assert from "node:assert/strict";
import test from "node:test";

import { assertMatchingVersions, goBuildArguments, targetToGo } from "./prepare-sidecar.mjs";

test("desktop targets map to the matching Go sidecar", () => {
  assert.deepEqual(targetToGo("x86_64-pc-windows-msvc"), {
    goos: "windows",
    goarch: "amd64",
    extension: ".exe",
  });
  assert.deepEqual(targetToGo("aarch64-apple-darwin"), {
    goos: "darwin",
    goarch: "arm64",
    extension: "",
  });
  assert.deepEqual(targetToGo("x86_64-unknown-linux-gnu"), {
    goos: "linux",
    goarch: "amd64",
    extension: "",
  });
  assert.throws(() => targetToGo("wasm32-unknown-unknown"), /unsupported/i);
});

test("native shell and Go core releases must match", () => {
  assert.doesNotThrow(() => assertMatchingVersions("0.1.33", 'const nodeVersion = "v0.1.33"'));
  assert.throws(
    () => assertMatchingVersions("0.1.33", 'const nodeVersion = "v0.1.32"'),
    /does not match/i,
  );
});

test("Windows sidecar keeps normal Go metadata for antivirus compatibility", () => {
  const args = goBuildArguments("C:\\build\\btc09-core.exe");
  assert.deepEqual(args, [
    "build",
    "-trimpath",
    "-o",
    "C:\\build\\btc09-core.exe",
    "./cmd/btc09",
  ]);
  assert.ok(!args.some((argument) => argument.includes("-s -w")));
});

test("store sidecars compile the restricted wallet edition", () => {
  assert.deepEqual(goBuildArguments("C:\\build\\btc09-core.exe", "wallet"), [
    "build",
    "-trimpath",
    "-tags",
    "walletedition",
    "-o",
    "C:\\build\\btc09-core.exe",
    "./cmd/btc09",
  ]);
});
