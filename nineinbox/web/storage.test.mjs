import assert from "node:assert/strict";
import test from "node:test";

import { createInboxStorage, createMemoryBackend } from "./storage.mjs";

function bundle() {
  return {
    v: 1,
    apiBase: "https://btc09.org",
    mailboxId: "EREREREREREREREREREREQ",
    key: new Uint8Array(32).fill(1),
    writeToken: new Uint8Array(32).fill(2),
    recoveryToken: new Uint8Array(16).fill(3),
  };
}

test("inbox secrets survive storage without sharing mutable key buffers", async () => {
  const storage = createInboxStorage(createMemoryBackend());
  const original = bundle();
  await storage.saveBundle(original);
  original.key.fill(9);

  const first = await storage.loadBundle();
  assert.equal(first.key[0], 1);
  first.writeToken.fill(8);
  const second = await storage.loadBundle();
  assert.equal(second.writeToken[0], 2);

  await storage.clearBundle();
  assert.equal(await storage.loadBundle(), null);
});

test("decrypted item cache is explicit, ordered, and removable", async () => {
  const storage = createInboxStorage(createMemoryBackend());
  const older = "EREREREREREREREREREREQ";
  const newer = "IiIiIiIiIiIiIiIiIiIiIg";
  await storage.putCachedItem({ id: older, createdAt: "2026-07-13T04:00:00.000Z", text: "one", data: new Uint8Array() });
  await storage.putCachedItem({ id: newer, createdAt: "2026-07-13T05:00:00.000Z", text: "two", data: new Uint8Array([1, 2]) });

  const items = await storage.listCachedItems();
  assert.deepEqual(items.map((item) => item.id), [newer, older]);
  items[0].data[0] = 9;
  assert.deepEqual((await storage.getCachedItem(newer)).data, new Uint8Array([1, 2]));

  await storage.deleteCachedItem(newer);
  assert.equal(await storage.getCachedItem(newer), null);
  assert.deepEqual((await storage.listCachedItems()).map((item) => item.id), [older]);
  await storage.clearCachedItems();
  assert.deepEqual(await storage.listCachedItems(), []);
});

test("device settings have bounded plain values and sensible defaults", async () => {
  const storage = createInboxStorage(createMemoryBackend());
  assert.deepEqual(await storage.loadSettings(), { deviceLabel: "This device", retention: "standard" });
  await storage.saveSettings({ deviceLabel: "Phone", retention: "pinned" });
  assert.deepEqual(await storage.loadSettings(), { deviceLabel: "Phone", retention: "pinned" });
  await assert.rejects(() => storage.saveSettings({ deviceLabel: "", retention: "standard" }), /settings/i);
  await assert.rejects(() => storage.saveSettings({ deviceLabel: "x".repeat(65), retention: "standard" }), /settings/i);
});

test("storage implementation never polls or reads the clipboard", async () => {
  const source = await import("node:fs/promises").then((fs) => fs.readFile(new URL("./storage.mjs", import.meta.url), "utf8"));
  assert.ok(!source.includes("setInterval"));
  assert.ok(!source.includes("clipboard.read"));
  assert.ok(!source.includes("clipboard.addEventListener"));
});
