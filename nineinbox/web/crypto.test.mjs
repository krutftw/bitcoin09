import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import test from "node:test";

if (!globalThis.crypto) globalThis.crypto = webcrypto;

import {
  MAX_CONTENT_BYTES,
  createItemId,
  createPairingBundle,
  decodePairingFragment,
  decryptItem,
  encodePairingFragment,
  encryptItem,
  hashToken,
  safetyWords,
} from "./crypto.mjs";

const mailboxId = "EREREREREREREREREREREQ";
const otherMailboxId = "IiIiIiIiIiIiIiIiIiIiIg";
const itemId = "MzMzMzMzMzMzMzMzMzMzMw";
const otherItemId = "RERERERERERERERERERERA";

function fixedBundle(fill = 1) {
  return {
    v: 1,
    apiBase: "https://btc09.org",
    mailboxId,
    key: new Uint8Array(32).fill(fill),
    writeToken: new Uint8Array(32).fill(fill + 1),
    recoveryToken: new Uint8Array(16).fill(fill + 2),
  };
}

function textInput(overrides = {}) {
  return {
    kind: "text",
    name: "",
    type: "text/plain",
    device: "Home PC",
    createdAt: "2026-07-13T04:00:00.000Z",
    expiresAt: "2026-07-20T04:00:00.000Z",
    text: "A useful note",
    data: new Uint8Array(),
    ...overrides,
  };
}

test("pairing fragments round-trip independent secrets without padding", () => {
  const first = createPairingBundle("https://btc09.org/");
  const second = createPairingBundle("https://btc09.org");
  first.mailboxId = createItemId();
  second.mailboxId = createItemId();

  assert.equal(first.v, 1);
  assert.equal(first.apiBase, "https://btc09.org");
  assert.equal(first.key.length, 32);
  assert.equal(first.writeToken.length, 32);
  assert.equal(first.recoveryToken.length, 16);
  assert.notDeepEqual(first.key, second.key);
  assert.notDeepEqual(first.writeToken, second.writeToken);
  assert.notDeepEqual(first.recoveryToken, second.recoveryToken);

  const fragment = encodePairingFragment(first);
  assert.match(fragment, /^[A-Za-z0-9_-]+$/);
  assert.ok(!fragment.includes("="));
  const decoded = decodePairingFragment(fragment);
  assert.equal(decoded.apiBase, first.apiBase);
  assert.equal(decoded.mailboxId, first.mailboxId);
  assert.deepEqual(decoded.key, first.key);
  assert.deepEqual(decoded.writeToken, first.writeToken);
  assert.deepEqual(decoded.recoveryToken, first.recoveryToken);
});

test("pairing decoder rejects malformed, padded, incomplete, and insecure bundles", () => {
  const bundle = fixedBundle();
  const valid = encodePairingFragment(bundle);
  for (const candidate of ["", "%%%", valid + "=", valid.slice(1), "eyJ2IjoxfQ"]) {
    assert.throws(() => decodePairingFragment(candidate), /pairing/i);
  }
  assert.throws(() => createPairingBundle("http://example.com"), /secure/i);
  assert.doesNotThrow(() => createPairingBundle("http://127.0.0.1:8020"));
});

test("token hashing and item IDs use fixed cryptographic sizes", async () => {
  const token = new Uint8Array(32).fill(9);
  const digest = await hashToken(token);
  assert.equal(digest.length, 32);
  assert.deepEqual(digest, new Uint8Array(await crypto.subtle.digest("SHA-256", token)));
  assert.match(createItemId(), /^[A-Za-z0-9_-]{22}$/);
  assert.notEqual(createItemId(), createItemId());
  await assert.rejects(() => hashToken(new Uint8Array(31)), /token/i);
});

test("safety words are deterministic and independent of device ordering", async () => {
  const bundle = fixedBundle();
  const first = await safetyWords(bundle, "local-challenge", "remote-challenge");
  const reversed = await safetyWords(bundle, "remote-challenge", "local-challenge");
  const changed = await safetyWords(bundle, "local-challenge", "different");
  assert.match(first, /^[a-z]+ [a-z]+$/);
  assert.equal(first, reversed);
  assert.notEqual(first, changed);
});

test("text and link items round-trip with a fresh nonce", async () => {
  const bundle = fixedBundle();
  for (const input of [textInput(), textInput({ kind: "link", type: "text/uri-list", text: "https://example.com/path" })]) {
    const first = await encryptItem(input, bundle, mailboxId, itemId);
    const second = await encryptItem(input, bundle, mailboxId, itemId);
    assert.notDeepEqual(first.slice(0, 12), second.slice(0, 12));
    assert.ok(first.length > input.text.length + 28);
    const decoded = await decryptItem(first, bundle, mailboxId, itemId);
    assert.equal(decoded.kind, input.kind);
    assert.equal(decoded.text, input.text);
    assert.equal(decoded.size, new TextEncoder().encode(input.text).length);
    assert.deepEqual(decoded.data, new Uint8Array());
  }
});

test("binary items preserve bytes without base64 expansion", async () => {
  const bundle = fixedBundle();
  const data = new Uint8Array(1024);
  crypto.getRandomValues(data);
  const input = textInput({
    kind: "file",
    name: "scan.pdf",
    type: "application/pdf",
    text: "",
    data,
  });
  const ciphertext = await encryptItem(input, bundle, mailboxId, itemId);
  assert.ok(ciphertext.length < data.length + 2048);
  const decoded = await decryptItem(ciphertext, bundle, mailboxId, itemId);
  assert.equal(decoded.name, "scan.pdf");
  assert.equal(decoded.type, "application/pdf");
  assert.equal(decoded.size, data.length);
  assert.deepEqual(decoded.data, data);
});

test("wrong keys, tampering, and wrong mailbox or item binding fail closed", async () => {
  const bundle = fixedBundle();
  const ciphertext = await encryptItem(textInput(), bundle, mailboxId, itemId);
  const tampered = ciphertext.slice();
  tampered[tampered.length - 1] ^= 1;

  await assert.rejects(() => decryptItem(ciphertext, fixedBundle(7), mailboxId, itemId), /decrypt/i);
  await assert.rejects(() => decryptItem(tampered, bundle, mailboxId, itemId), /decrypt/i);
  await assert.rejects(() => decryptItem(ciphertext, bundle, otherMailboxId, itemId), /decrypt/i);
  await assert.rejects(() => decryptItem(ciphertext, bundle, mailboxId, otherItemId), /decrypt/i);
});

test("encryption refuses a mailbox ID different from the pairing bundle", async () => {
  await assert.rejects(
    () => encryptItem(textInput(), fixedBundle(), otherMailboxId, itemId),
    /mailbox/i,
  );
});

test("invalid metadata and content over 20 MiB are rejected before encryption", async () => {
  const bundle = fixedBundle();
  const invalid = [
    textInput({ kind: "wat" }),
    textInput({ device: "" }),
    textInput({ createdAt: "not-a-time" }),
    textInput({ expiresAt: "2026-07-12T04:00:00.000Z" }),
    textInput({ kind: "file", name: "", text: "", data: new Uint8Array([1]) }),
  ];
  for (const input of invalid) {
    await assert.rejects(() => encryptItem(input, bundle, mailboxId, itemId), /item/i);
  }
  await assert.rejects(
    () => encryptItem(textInput({ kind: "file", name: "large.bin", text: "", data: new Uint8Array(MAX_CONTENT_BYTES + 1) }), bundle, mailboxId, itemId),
    /20 MiB/i,
  );
});
