import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  XP_RANKS,
  XpStore,
  formatLeaderboard,
  formatRankSummary,
  levelForXp,
  rankForLevel,
  xpForLevel,
} from "./xp-system.mjs";

test("activity levels use predictable quadratic thresholds", () => {
  assert.equal(xpForLevel(0), 0);
  assert.equal(xpForLevel(1), 100);
  assert.equal(xpForLevel(3), 900);
  assert.equal(levelForXp(0), 0);
  assert.equal(levelForXp(99), 0);
  assert.equal(levelForXp(100), 1);
  assert.equal(levelForXp(899), 2);
  assert.equal(levelForXp(900), 3);
});

test("rank tiers stay small and only return the highest earned role", () => {
  assert.deepEqual(XP_RANKS.map(({ level, name }) => ({ level, name })), [
    { level: 3, name: "🌱 Active" },
    { level: 8, name: "⭐ Regular" },
    { level: 15, name: "🏆 Veteran" },
  ]);
  assert.equal(rankForLevel(2), null);
  assert.equal(rankForLevel(3)?.name, "🌱 Active");
  assert.equal(rankForLevel(14)?.name, "⭐ Regular");
  assert.equal(rankForLevel(15)?.name, "🏆 Veteran");
});

test("XP ignores bots, webhooks, DMs, and message spam", async () => {
  const directory = await mkdtemp(join(tmpdir(), "btc09-xp-"));
  try {
    let now = 1_000_000;
    const store = new XpStore({
      filePath: join(directory, "xp.json"),
      now: () => now,
      random: () => 0.5,
    });
    const message = {
      guild_id: "guild-1",
      channel_id: "channel-1",
      type: 0,
      author: { id: "user-1", bot: false },
    };

    assert.equal((await store.awardForMessage({ ...message, guild_id: undefined })).awarded, 0);
    assert.equal((await store.awardForMessage({ ...message, webhook_id: "hook-1" })).awarded, 0);
    assert.equal((await store.awardForMessage({ ...message, author: { id: "bot-1", bot: true } })).awarded, 0);
    assert.equal((await store.awardForMessage({ ...message, type: 7 })).awarded, 0);

    const first = await store.awardForMessage(message);
    assert.equal(first.awarded, 20);
    assert.equal(first.totalXp, 20);
    now += 59_999;
    assert.equal((await store.awardForMessage(message)).awarded, 0);
    now += 1;
    const second = await store.awardForMessage(message);
    assert.equal(second.awarded, 20);
    assert.equal(second.totalXp, 40);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("XP survives a restart and leaderboard output is compact", async () => {
  const directory = await mkdtemp(join(tmpdir(), "btc09-xp-"));
  const filePath = join(directory, "xp.json");
  try {
    let now = 2_000_000;
    const store = new XpStore({ filePath, now: () => now, random: () => 0.5 });
    const messageFor = (id) => ({
      guild_id: "guild-1",
      channel_id: "channel-1",
      type: 0,
      author: { id, bot: false },
    });
    await store.awardForMessage(messageFor("user-1"));
    now += 60_000;
    await store.awardForMessage(messageFor("user-1"));
    await store.awardForMessage(messageFor("user-2"));

    const restarted = new XpStore({ filePath });
    await restarted.load();
    assert.equal(restarted.getMember("user-1").xp, 40);
    assert.deepEqual(restarted.leaderboard(2).map(({ userId, xp }) => ({ userId, xp })), [
      { userId: "user-1", xp: 40 },
      { userId: "user-2", xp: 20 },
    ]);
    assert.equal(
      formatRankSummary("user-1", { xp: 900 }),
      "<@user-1> • Level 3 • 900 XP\n700 XP to Level 4",
    );
    assert.equal(
      formatLeaderboard(restarted.leaderboard(2)),
      "09C activity leaderboard\n1. <@user-1> - Level 0 (40 XP)\n2. <@user-2> - Level 0 (20 XP)",
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
