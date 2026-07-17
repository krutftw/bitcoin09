import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";

import { assertNoBuildPathLeaks, bundlePaths } from "./verify-macos-bundle.mjs";

test("macOS bundle paths include the native shell and bundled wallet core", () => {
  const paths = bundlePaths("/tmp/BTC09 Wallet.app");
  assert.equal(paths.infoPlist, path.join("/tmp/BTC09 Wallet.app", "Contents", "Info.plist"));
  assert.equal(paths.shell, path.join("/tmp/BTC09 Wallet.app", "Contents", "MacOS", "btc09-wallet"));
  assert.equal(paths.sidecar, path.join("/tmp/BTC09 Wallet.app", "Contents", "MacOS", "btc09-core"));
});

test("macOS verification rejects local and CI build paths", () => {
  assert.doesNotThrow(() => assertNoBuildPathLeaks(Buffer.from("clean release binary"), ["/Users/cirrus/project"]));
  assert.throws(
    () => assertNoBuildPathLeaks(Buffer.from("panic at /Users/cirrus/project/walletapp/src-tauri/src/main.rs"), ["/Users/cirrus/project"]),
    /build path/i,
  );
  assert.throws(
    () => assertNoBuildPathLeaks(Buffer.from("C:\\projects\\bitcoin09\\walletapp"), []),
    /build path/i,
  );
});
