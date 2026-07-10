import assert from "node:assert/strict";
import test from "node:test";
import { redactDiscordPath } from "./discord-api.mjs";

test("Discord interaction and webhook tokens are redacted from log paths", () => {
  assert.equal(
    redactDiscordPath("/interactions/123/a.b-token/callback"),
    "/interactions/123/[redacted]/callback",
  );
  assert.equal(
    redactDiscordPath("/webhooks/client-id/a.b-token/messages/@original"),
    "/webhooks/client-id/[redacted]/messages/@original",
  );
  assert.equal(
    redactDiscordPath("/guilds/guild-id/members/user-id"),
    "/guilds/guild-id/members/user-id",
  );
});
