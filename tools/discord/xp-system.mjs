import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

export const XP_COOLDOWN_MS = 60_000;
export const XP_RANKS = [
  { level: 3, name: "🌱 Active", color: 0x57f287 },
  { level: 8, name: "⭐ Regular", color: 0x5865f2 },
  { level: 15, name: "🏆 Veteran", color: 0xfee75c },
];

export function xpForLevel(level) {
  const safeLevel = Math.max(0, Math.trunc(Number(level) || 0));
  return 100 * safeLevel * safeLevel;
}

export function levelForXp(xp) {
  const safeXp = Math.max(0, Math.trunc(Number(xp) || 0));
  return Math.floor(Math.sqrt(safeXp / 100));
}

export function rankForLevel(level) {
  const safeLevel = Math.max(0, Math.trunc(Number(level) || 0));
  return XP_RANKS.filter((rank) => rank.level <= safeLevel).at(-1) ?? null;
}

export function formatRankSummary(userId, member) {
  const xp = Math.max(0, Math.trunc(Number(member?.xp) || 0));
  const level = levelForXp(xp);
  const nextLevelXp = xpForLevel(level + 1);
  return `<@${userId}> • Level ${level} • ${xp.toLocaleString()} XP\n` +
    `${(nextLevelXp - xp).toLocaleString()} XP to Level ${level + 1}`;
}

export function formatLeaderboard(entries) {
  if (!entries.length) return "No activity XP yet. Start chatting and check back soon.";
  return [
    "09C activity leaderboard",
    ...entries.map((entry, index) =>
      `${index + 1}. <@${entry.userId}> - Level ${entry.level} (${entry.xp.toLocaleString()} XP)`
    ),
  ].join("\n");
}

export class XpStore {
  constructor({ filePath, now = Date.now, random = Math.random } = {}) {
    if (!filePath) throw new Error("XP state file path is required.");
    this.filePath = filePath;
    this.now = now;
    this.random = random;
    this.loaded = false;
    this.members = {};
    this.queue = Promise.resolve();
  }

  async load() {
    if (this.loaded) return;
    try {
      const parsed = JSON.parse(await readFile(this.filePath, "utf8"));
      this.members = sanitizeMembers(parsed?.members);
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
      this.members = {};
    }
    this.loaded = true;
  }

  async awardForMessage(message) {
    if (!isEligibleMessage(message)) return emptyAward();
    const run = this.queue.then(() => this.#awardEligibleMessage(message));
    this.queue = run.catch(() => {});
    return run;
  }

  async #awardEligibleMessage(message) {
    await this.load();
    const userId = message.author.id;
    const now = Math.trunc(Number(this.now()));
    const previous = this.members[userId] ?? { xp: 0, lastAwardAt: null };
    if (
      previous.lastAwardAt != null &&
      now - previous.lastAwardAt < XP_COOLDOWN_MS
    ) {
      return {
        ...emptyAward(),
        userId,
        totalXp: previous.xp,
        level: levelForXp(previous.xp),
      };
    }

    const oldLevel = levelForXp(previous.xp);
    const awarded = 15 + Math.floor(normalizedRandom(this.random()) * 11);
    const totalXp = previous.xp + awarded;
    const level = levelForXp(totalXp);
    this.members[userId] = { xp: totalXp, lastAwardAt: now };
    await this.#save();
    return {
      awarded,
      userId,
      totalXp,
      oldLevel,
      level,
      leveledUp: level > oldLevel,
    };
  }

  getMember(userId) {
    const member = this.members[userId] ?? { xp: 0, lastAwardAt: null };
    return {
      userId,
      xp: member.xp,
      level: levelForXp(member.xp),
      lastAwardAt: member.lastAwardAt,
    };
  }

  leaderboard(limit = 10) {
    const safeLimit = Math.max(1, Math.min(25, Math.trunc(Number(limit) || 10)));
    return Object.entries(this.members)
      .map(([userId, member]) => ({
        userId,
        xp: member.xp,
        level: levelForXp(member.xp),
      }))
      .sort((left, right) => right.xp - left.xp || left.userId.localeCompare(right.userId))
      .slice(0, safeLimit);
  }

  async #save() {
    await mkdir(dirname(this.filePath), { recursive: true });
    const temporaryPath = `${this.filePath}.${process.pid}.tmp`;
    await writeFile(
      temporaryPath,
      `${JSON.stringify({ version: 1, members: this.members }, null, 2)}\n`,
      { encoding: "utf8", mode: 0o600 },
    );
    await rename(temporaryPath, this.filePath);
  }
}

function isEligibleMessage(message) {
  return Boolean(
    message?.guild_id &&
    message?.channel_id &&
    message?.author?.id &&
    !message.author.bot &&
    !message.webhook_id &&
    (message.type === 0 || message.type === 19),
  );
}

function sanitizeMembers(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  const members = {};
  for (const [userId, member] of Object.entries(value)) {
    if (!/^\d+$/.test(userId) && !/^user-/.test(userId)) continue;
    const xp = Math.max(0, Math.trunc(Number(member?.xp) || 0));
    const lastAwardAt = member?.lastAwardAt == null
      ? null
      : Math.max(0, Math.trunc(Number(member.lastAwardAt) || 0));
    members[userId] = { xp, lastAwardAt };
  }
  return members;
}

function normalizedRandom(value) {
  const number = Number(value);
  if (!Number.isFinite(number)) return 0;
  return Math.min(0.999999999, Math.max(0, number));
}

function emptyAward() {
  return {
    awarded: 0,
    userId: null,
    totalXp: 0,
    oldLevel: 0,
    level: 0,
    leveledUp: false,
  };
}
