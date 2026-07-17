import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

test("store builds are wallet-only and use separate product copy", async () => {
  const packageJSON = JSON.parse(await readFile(path.join(root, "walletapp", "package.json"), "utf8"));
  assert.match(packageJSON.scripts["store:build"], /--features wallet-only/);
  assert.match(packageJSON.scripts["store:build"], /tauri\.store\.conf\.json/);

  const storeConfig = JSON.parse(
    await readFile(path.join(root, "walletapp", "src-tauri", "tauri.store.conf.json"), "utf8"),
  );
  const description = `${storeConfig.bundle.shortDescription} ${storeConfig.bundle.longDescription}`;
  assert.match(description, /send, receive/i);
  assert.doesNotMatch(description, /min(e|er|ing)/i);
  assert.match(storeConfig.build.beforeBuildCommand, /prepare-sidecar\.mjs wallet/);
});

test("Android release builds both the test APK and Play Store bundle", async () => {
  const packageJSON = JSON.parse(await readFile(path.join(root, "walletapp", "package.json"), "utf8"));
  const command = packageJSON.scripts["mobile:android:build"];

  assert.match(command, /android build/);
  assert.match(command, /--apk/);
  assert.match(command, /--aab/);
  assert.match(command, /--target aarch64/);
  assert.match(command, /--ci/);
  assert.match(command, /mobile:android:verify/);
});

test("macOS release builds one wallet-only app for Intel and Apple Silicon", async () => {
  const packageJSON = JSON.parse(await readFile(path.join(root, "walletapp", "package.json"), "utf8"));
  const command = packageJSON.scripts["macos:universal:build"];

  assert.match(command, /--features wallet-only/);
  assert.match(command, /tauri\.store\.conf\.json/);
  assert.match(command, /--target universal-apple-darwin/);
  assert.match(command, /--bundles app/);
});

test("the native startup error can close the real app window", async () => {
  const startup = await readFile(path.join(root, "walletapp", "src", "startup.js"), "utf8");
  assert.match(startup, /window\.__TAURI__\.core\.invoke\("close_wallet"\)/);
});
