import assert from "node:assert/strict";
import test from "node:test";
import path from "node:path";
import { selectReleaseApk } from "./verify-release-outputs.mjs";

test("Android release verification accepts one signed output", () => {
  const directory = path.resolve("android-output");
  const signed = path.join(directory, "app-universal-release.apk");
  assert.deepEqual(
    selectReleaseApk(directory, (candidate) => candidate === signed),
    { path: signed, signed: true },
  );
});

test("Android release verification accepts one unsigned CI output", () => {
  const directory = path.resolve("android-output");
  const unsigned = path.join(directory, "app-universal-release-unsigned.apk");
  assert.deepEqual(
    selectReleaseApk(directory, (candidate) => candidate === unsigned),
    { path: unsigned, signed: false },
  );
});

test("Android release verification rejects missing or stale ambiguous outputs", () => {
  const directory = path.resolve("android-output");
  assert.throws(
    () => selectReleaseApk(directory, () => false),
    /exactly one fresh signed or unsigned/,
  );
  assert.throws(
    () => selectReleaseApk(directory, () => true),
    /exactly one fresh signed or unsigned/,
  );
});
