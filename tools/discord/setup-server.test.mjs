import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./setup-server.mjs", import.meta.url), "utf8");

test("v0.1.32 announcement explains the wallet changes without hype", () => {
  const start = source.indexOf('marker: "Bitcoin 09 v0.1.32 is out."');
  assert.ok(start >= 0, "missing v0.1.32 announcement");
  const announcement = source.slice(start, start + 1800);
  for (const required of [
    "Bitcoin 09 v0.1.32 is out.",
    "Activity tab",
    "Max",
    "Combine small payments",
    "mining rewards waiting",
    "https://github.com/krutftw/bitcoin09/releases/tag/v0.1.32",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["revolutionary", "game-changing", "seamless", "guaranteed", "profit", "—"]) {
    assert.ok(!announcement.includes(forbidden), `announcement contains ${forbidden}`);
  }
});

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

test("v0.1.27 announcement explains recovery wallets without hype", () => {
  const start = source.indexOf('marker: "Bitcoin 09 v0.1.27 is out."');
  assert.ok(start >= 0, "missing v0.1.27 announcement");
  const announcement = source.slice(start, start + 1800);
  for (const required of [
    "Bitcoin 09 v0.1.27 is out.",
    "24 recovery words",
    "encrypted local wallet file",
    "same address",
    "Existing wallets are left alone",
    "https://github.com/krutftw/bitcoin09/releases/tag/v0.1.27",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["revolutionary", "game-changing", "seamless", "guaranteed", "—"]) {
    assert.ok(!announcement.includes(forbidden), `announcement contains ${forbidden}`);
  }
});

test("v0.1.28 announcement explains PPLNS without hype", () => {
  const start = source.indexOf('marker: "Bitcoin 09 v0.1.28 is out."');
  assert.ok(start >= 0, "missing v0.1.28 announcement");
  const announcement = source.slice(start, start + 1800);
  for (const required of [
    "Bitcoin 09 v0.1.28 is out.",
    "PPLNS",
    "0% pool fee",
    "directly from the block coinbase",
    "100 confirmations",
    "https://github.com/krutftw/bitcoin09/releases/tag/v0.1.28",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["revolutionary", "game-changing", "seamless", "guaranteed", "profit", "—"]) {
    assert.ok(!announcement.includes(forbidden), `announcement contains ${forbidden}`);
  }
});

test("server status post names v0.1.31 as the current release", () => {
  assert.ok(source.includes('"Current release: v0.1.31"'));
});

test("v0.1.31 announcement explains the sync fix without blaming miners", () => {
  for (const text of [
    "Bitcoin 09 v0.1.31 is out.",
    "The public node was falling behind during the recent burst of fast blocks.",
    "solo blocks from distributed/PPLNS payouts",
    "nobody is being blocked for finding lots of valid blocks",
    "https://github.com/krutftw/bitcoin09/releases/tag/v0.1.31",
  ]) {
    assert.ok(source.includes(text), `missing release wording: ${text}`);
  }
});

test("mining support update points people to the official client only", () => {
  const start = source.indexOf('marker: "Mining support update"');
  assert.ok(start >= 0, "missing mining support update");
  const announcement = source.slice(start, start + 1400);
  for (const required of [
    "Mining support update",
    "Download BTC09 only from the official GitHub releases page.",
    "open-source CPU miner",
    "non-custodial PPLNS",
    "No GPU miner is currently endorsed by the project.",
    "https://github.com/krutftw/bitcoin09/releases",
  ]) {
    assert.ok(announcement.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["ntmminer", "mediafire", "bitcoin09.tutuit.xyz", "—"]) {
    assert.ok(!source.toLowerCase().includes(forbidden), `setup contains ${forbidden}`);
  }
});

test("mining help describes the current PPLNS default", () => {
  const start = source.indexOf('marker: "Official BTC09 wallet miner"');
  assert.ok(start >= 0, "missing wallet miner help post");
  const helpPost = source.slice(start, start + 1600);
  for (const required of [
    "non-custodial PPLNS",
    "0% pool fee",
    "directly from the block coinbase",
    "100 confirmations",
    "Remote solo",
  ]) {
    assert.ok(helpPost.includes(required), `missing ${required}`);
  }
  assert.ok(!helpPost.includes("It is open solo mining"));
});
