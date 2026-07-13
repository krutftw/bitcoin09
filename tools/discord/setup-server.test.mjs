import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./setup-server.mjs", import.meta.url), "utf8");

test("Nine Inbox announcement is concise, honest, and English", () => {
  for (const required of [
    'marker: "Nine Inbox is live."',
    '"Nine Inbox is live."',
    '"https://btc09.org/inbox/"',
    "No account and no 09C needed.",
    "Items are encrypted in your browser before upload.",
    "Files last 7 days; pinned text and links last 30 days.",
    "Closed-browser push is not part of this version.",
    "Post bugs in #🐞-bug-reports.",
  ]) {
    assert.ok(source.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["revolutionary", "game-changing", "seamless", "guaranteed", "—"] ) {
    assert.ok(!source.slice(source.indexOf('marker: "Nine Inbox is live."'), source.indexOf('marker: "Nine Inbox is live."') + 1200).includes(forbidden));
  }
});

test("v0.1.26 announcement explains the useful changes without hype", () => {
  const start = source.indexOf('marker: "Bitcoin 09 v0.1.26 is out."');
  assert.ok(start >= 0, "missing v0.1.26 announcement");
  const announcement = source.slice(start, start + 1800);
  for (const required of [
    "Bitcoin 09 v0.1.26 is out.",
    "Nine Inbox",
    "Copy help report",
    "Go 1.25.12",
    "https://github.com/krutftw/bitcoin09/releases/tag/v0.1.26",
    "Open solo is still solo mining",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["revolutionary", "game-changing", "seamless", "guaranteed", "—"]) {
    assert.ok(!announcement.includes(forbidden), `announcement contains ${forbidden}`);
  }
});

test("mining support update points people to the official client only", () => {
  const start = source.indexOf('marker: "Mining support update"');
  assert.ok(start >= 0, "missing mining support update");
  const announcement = source.slice(start, start + 1400);
  for (const required of [
    "Mining support update",
    "Download BTC09 only from the official GitHub releases page.",
    "open-source CPU solo miner",
    "Pooled payouts are not live in the official software.",
    "No pool or GPU miner is currently endorsed by the project.",
    "https://github.com/krutftw/bitcoin09/releases",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["ntmminer", "mediafire", "bitcoin09.tutuit.xyz", "—"]) {
    assert.ok(!source.toLowerCase().includes(forbidden), `setup contains ${forbidden}`);
  }
});
