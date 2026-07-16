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
});

test("the native startup error can close the real app window", async () => {
  const startup = await readFile(path.join(root, "walletapp", "src", "startup.js"), "utf8");
  assert.match(startup, /window\.__TAURI__\.core\.invoke\("close_wallet"\)/);
});
