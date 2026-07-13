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
