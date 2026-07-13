import assert from "node:assert/strict";
import test from "node:test";

import { encodeQR } from "./qr.mjs";

test("pairing QR encoder returns a square Version 11 matrix", () => {
  const matrix = encodeQR("https://btc09.org/inbox/#pair=" + "A".repeat(280));
  assert.equal(matrix.length, 61);
  assert.ok(matrix.every((row) => row.length === 61 && row.every((cell) => typeof cell === "boolean")));
  assert.deepEqual(matrix.slice(0, 7).map((row) => row.slice(0, 7)), [
    [true, true, true, true, true, true, true],
    [true, false, false, false, false, false, true],
    [true, false, true, true, true, false, true],
    [true, false, true, true, true, false, true],
    [true, false, true, true, true, false, true],
    [true, false, false, false, false, false, true],
    [true, true, true, true, true, true, true],
  ]);
});

test("pairing QR rejects content beyond its reviewed byte capacity", () => {
  assert.throws(() => encodeQR("A".repeat(322)), /too long/i);
  assert.doesNotThrow(() => encodeQR("A".repeat(321)));
});
