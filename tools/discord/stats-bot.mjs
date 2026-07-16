#!/usr/bin/env node

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { redactDiscordPath } from "./discord-api.mjs";
import { DiscordGatewayWatcher, fetchGatewayWithRetry } from "./gateway-watcher.mjs";
import {
  XP_RANKS,
  XpStore,
  formatLeaderboard,
  formatRankSummary,
  rankForLevel,
} from "./xp-system.mjs";

const API_BASE = "https://discord.com/api/v10";
const EXPLORER_STATUS = "https://explorer.btc09.org/api/status";
const DISCORD_INVITE = "https://discord.gg/fUuGzwRTzP";
const MESSAGE_MARKER = "Bitcoin 09 live mining stats";
const DEFAULT_STATS_CHANNEL = "pools-and-nodes";
const LIVE_STATS_CATEGORY = "📊 LIVE STATS";
const LIVE_STATS_REFRESH_MS = 600_000;
const CHANNEL_TYPE_VOICE = 2;
const CHANNEL_TYPE_CATEGORY = 4;
const CONNECT_PERMISSION = "1048576";
const scriptDir = dirname(fileURLToPath(import.meta.url));
const selfAssignableRoles = [
  { key: "miner", label: "Miner", roleName: "⛏ Miner" },
  { key: "node_operator", label: "Node Operator", roleName: "🧱 Node Operator" },
  { key: "updates", label: "Updates", roleName: "🔔 Updates" },
  { key: "contributor", label: "Contributor", roleName: "🤝 Contributor" },
  { key: "tester", label: "Tester", roleName: "🧪 Tester" },
];
let roleCache = null;
let activityXp = null;

loadLocalEnv();

const args = new Set(process.argv.slice(2).filter((arg) => !arg.startsWith("--channel=")));
const channelArg = process.argv
  .slice(2)
  .find((arg) => arg.startsWith("--channel="))
  ?.slice("--channel=".length);

if (args.has("--help")) {
  usage();
  process.exit(0);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message || error);
    process.exitCode = error.exitCode ?? 1;
  });
}

async function main() {
  if (args.size === 0) {
    const stats = await getStats();
    console.log(formatStatsMessage(stats));
    console.log("");
    usage();
    return;
  }

  if (args.has("--register-commands")) {
    await registerCommands();
  }

  if (args.has("--post")) {
    await postOrUpdateStatsMessage();
  }

  if (args.has("--watch")) {
    await watchGateway();
  }
}

async function registerCommands() {
  requireDiscordEnv();

  const commands = getCommandDefinitions();

  const registered = [];
  for (const command of commands) {
    registered.push(await discord("POST", `/applications/${process.env.DISCORD_CLIENT_ID}/guilds/${process.env.DISCORD_GUILD_ID}/commands`, command));
  }
  for (const command of registered) {
    console.log(`Registered /${command.name} (${command.id}) in guild ${process.env.DISCORD_GUILD_ID}.`);
  }
}

export function getCommandDefinitions() {
  return [
    {
      name: "stats",
      description: "Show live Bitcoin 09 mining and network stats.",
      type: 1,
    },
    {
      name: "rank",
      description: "Show your Bitcoin 09 community activity level.",
      type: 1,
    },
    {
      name: "leaderboard",
      description: "Show the Bitcoin 09 community activity leaderboard.",
      type: 1,
    },
    {
      name: "wallet",
      description: "Open the current Bitcoin 09 wallet guide.",
      type: 1,
    },
    {
      name: "mine",
      description: "Open the current Bitcoin 09 mining guide.",
      type: 1,
    },
  ];
}

async function postOrUpdateStatsMessage() {
  requireDiscordEnv();

  const channels = await discord("GET", `/guilds/${process.env.DISCORD_GUILD_ID}/channels`);
  const channel = findStatsChannel(channels, channelArg ?? DEFAULT_STATS_CHANNEL);
  if (!channel) {
    throw new Error(`Could not find stats channel ${channelArg ?? DEFAULT_STATS_CHANNEL}. Use --channel=<id-or-name>.`);
  }

  const botUser = await discord("GET", "/users/@me");
  const stats = await getStats();
  const content = formatStatsMessage(stats);
  const recentMessages = await discord("GET", `/channels/${channel.id}/messages?limit=50`);
  const existing = recentMessages.find(
    (message) => message.author?.id === botUser.id && message.content.includes(MESSAGE_MARKER),
  );

  if (existing) {
    await discord("PATCH", `/channels/${channel.id}/messages/${existing.id}`, {
      content,
      allowed_mentions: { parse: [] },
    });
    console.log(`Updated live stats message in #${channel.name}.`);
    return;
  }

  await discord("POST", `/channels/${channel.id}/messages`, {
    content,
    allowed_mentions: { parse: [] },
  });
  console.log(`Posted live stats message in #${channel.name}.`);
}

async function watchGateway() {
  requireDiscordEnv({ terminal: true });
  if (typeof WebSocket !== "function") {
    throw terminalStartupError("This Node runtime does not provide WebSocket. Use Node 22+ or install a websocket client.");
  }

  activityXp = new XpStore({ filePath: xpStateFile() });
  await activityXp.load();
  await syncXpRankRoles(process.env.DISCORD_GUILD_ID).catch((error) => {
    console.error("XP rank role setup failed:", error.message || error);
  });
  startLiveStatsUpdater();
  const gateway = await fetchGatewayWithRetry(
    () => discord("GET", "/gateway/bot"),
    { logger: console },
  );
  console.log("Connecting Discord gateway for role buttons...");
  const watcher = new DiscordGatewayWatcher({
    gatewayUrl: gateway.url,
    token: process.env.DISCORD_BOT_TOKEN,
    WebSocketCtor: WebSocket,
    logger: console,
    onDispatch: async (packet) => {
      if (packet.t === "INTERACTION_CREATE") {
        await handleInteraction(packet.d).catch((error) => {
          console.error("Interaction failed:", error.message || error);
        });
      }
      if (packet.t === "MESSAGE_CREATE") {
        await handleXpMessage(packet.d).catch((error) => {
          console.error("XP update failed:", error.message || error);
        });
      }
    },
    onFatal: (decision) => {
      process.exitCode = decision.exitCode;
    },
  });
  watcher.start();
}

async function handleInteraction(interaction) {
	const action = classifyInteraction(interaction);
	if (action === "stats") await handleStatsInteraction(interaction);
	if (action === "rank") await handleRankInteraction(interaction);
	if (action === "leaderboard") await handleLeaderboardInteraction(interaction);
	if (action === "wallet") await respondToInteraction(interaction, formatWalletHelp(), true);
	if (action === "mine") await respondToInteraction(interaction, formatMiningHelp(), true);
	if (action === "role") await handleRoleButtonInteraction(interaction);
}

export function classifyInteraction(interaction) {
	if (interaction?.type === 2 && interaction.data?.name === "stats") return "stats";
	if (interaction?.type === 2 && interaction.data?.name === "rank") return "rank";
	if (interaction?.type === 2 && interaction.data?.name === "leaderboard") return "leaderboard";
	if (interaction?.type === 2 && interaction.data?.name === "wallet") return "wallet";
	if (interaction?.type === 2 && interaction.data?.name === "mine") return "mine";
	if (interaction?.type === 3 && interaction.data?.custom_id?.startsWith("role:toggle:")) return "role";
	return null;
}

export function formatWalletHelp() {
  return [
    "**BTC09 Wallet**",
    "Open the BTC09 Wallet: https://btc09.org/#download",
    "Create a wallet or restore it with your 24 recovery words. Send, receive, Activity, backup, and mining are all in the app.",
    "Keep recovery words offline. Nobody from BTC09 will ask for them.",
  ].join("\n");
}

export function formatMiningHelp() {
  return [
    "**Mine 09C**",
    "Open the BTC09 Wallet and choose the Mine tab. Pick a sensible CPU thread count, then start.",
    "The official PPLNS pool has a 0% pool fee and pays accepted shares directly from a block. Rewards need 100 confirmations before they can be sent.",
    "If it fails, use Copy help report in the Mine tab and post it in mining help.",
    "Guide: https://btc09.org/#mining-guide",
  ].join("\n");
}

async function handleStatsInteraction(interaction) {
  await discord("POST", `/interactions/${interaction.id}/${interaction.token}/callback`, {
    type: 5,
  }, { auth: false });

  try {
    const stats = await getStats();
    await discord("PATCH", `/webhooks/${process.env.DISCORD_CLIENT_ID}/${interaction.token}/messages/@original`, {
      content: formatStatsMessage(stats),
      allowed_mentions: { parse: [] },
    }, { auth: false });
  } catch (error) {
    await discord("PATCH", `/webhooks/${process.env.DISCORD_CLIENT_ID}/${interaction.token}/messages/@original`, {
      content: "Could not load live 09C stats right now. Try again in a minute.",
      allowed_mentions: { parse: [] },
    }, { auth: false }).catch(() => {});
    throw error;
  }
}

async function handleRankInteraction(interaction) {
  const userId = interaction.member?.user?.id ?? interaction.user?.id;
  if (!userId || !activityXp) {
    await respondToInteraction(interaction, "Activity ranks are starting up. Try again in a moment.", true);
    return;
  }
  await respondToInteraction(
    interaction,
    formatRankSummary(userId, activityXp.getMember(userId)),
    true,
  );
}

async function handleLeaderboardInteraction(interaction) {
  if (!activityXp) {
    await respondToInteraction(interaction, "Activity ranks are starting up. Try again in a moment.", true);
    return;
  }
  await respondToInteraction(
    interaction,
    formatLeaderboard(activityXp.leaderboard(10)),
    false,
  );
}

async function respondToInteraction(interaction, content, ephemeral) {
  await discord("POST", `/interactions/${interaction.id}/${interaction.token}/callback`, {
    type: 4,
    data: {
      content,
      flags: ephemeral ? 64 : undefined,
      allowed_mentions: { parse: [] },
    },
  }, { auth: false });
}

async function handleXpMessage(message) {
  if (!activityXp || message?.guild_id !== process.env.DISCORD_GUILD_ID) return;
  const result = await activityXp.awardForMessage(message);
  if (!result.awarded) return;

  const oldRank = rankForLevel(result.oldLevel);
  const newRank = rankForLevel(result.level);
  if (oldRank?.name === newRank?.name) return;

  const roles = await syncXpRankRoles(message.guild_id);
  const memberRoleIds = Array.isArray(message.member?.roles)
    ? message.member.roles
    : await getMemberRoleIds(message.guild_id, result.userId);
  await applyXpRankRoles({
    guildId: message.guild_id,
    userId: result.userId,
    memberRoleIds,
    level: result.level,
    roles,
  });
}

async function handleRoleButtonInteraction(interaction) {
  await discord("POST", `/interactions/${interaction.id}/${interaction.token}/callback`, {
    type: 5,
    data: { flags: 64 },
  }, { auth: false });

  const roleKey = interaction.data.custom_id.slice("role:toggle:".length);
  const roleConfig = selfAssignableRoles.find((role) => role.key === roleKey);
  const userId = interaction.member?.user?.id ?? interaction.user?.id;
  const guildId = interaction.guild_id ?? process.env.DISCORD_GUILD_ID;

  if (!roleConfig || !userId || !guildId) {
    await editInteraction(interaction.token, "I could not understand that role button.");
    return;
  }

  const role = await findGuildRole(guildId, roleConfig.roleName);
  if (!role) {
    await editInteraction(interaction.token, `I could not find the ${roleConfig.label} role.`);
    return;
  }

  const memberRoleIds = await getMemberRoleIds(guildId, userId, interaction.member);
  const hasRole = memberRoleIds.includes(role.id);
  const method = hasRole ? "DELETE" : "PUT";
  await discord(method, `/guilds/${guildId}/members/${userId}/roles/${role.id}`, null);
  await editInteraction(
    interaction.token,
    hasRole
      ? `Removed ${roleConfig.roleName}.`
      : `Added ${roleConfig.roleName}.`,
  );
}

async function editInteraction(token, content) {
  await discord("PATCH", `/webhooks/${process.env.DISCORD_CLIENT_ID}/${token}/messages/@original`, {
    content,
    allowed_mentions: { parse: [] },
  }, { auth: false });
}

async function findGuildRole(guildId, roleName) {
  const now = Date.now();
  if (!roleCache || roleCache.guildId !== guildId || roleCache.expiresAt < now) {
    roleCache = {
      guildId,
      expiresAt: now + 60_000,
      roles: await discord("GET", `/guilds/${guildId}/roles`),
    };
  }

  return roleCache.roles.find((role) => role.name === roleName) ??
    roleCache.roles.find((role) => normalizeRoleName(role.name) === normalizeRoleName(roleName));
}

async function getMemberRoleIds(guildId, userId, interactionMember) {
  if (Array.isArray(interactionMember?.roles)) {
    return interactionMember.roles;
  }

  const member = await discord("GET", `/guilds/${guildId}/members/${userId}`);
  return member.roles ?? [];
}

export async function syncXpRankRoles(
  guildId,
  { discordImpl = discord } = {},
) {
  const guildRoles = await discordImpl("GET", `/guilds/${guildId}/roles`);
  const rankRoles = [];
  for (const rank of XP_RANKS) {
    let role = guildRoles.find(
      (candidate) => normalizeRoleName(candidate.name) === normalizeRoleName(rank.name),
    );
    if (!role) {
      role = await discordImpl("POST", `/guilds/${guildId}/roles`, {
        name: rank.name,
        color: rank.color,
        hoist: true,
        mentionable: false,
        permissions: "0",
      });
      guildRoles.push(role);
      roleCache = null;
    }
    rankRoles.push(role);
  }
  return rankRoles;
}

export async function applyXpRankRoles({
  guildId,
  userId,
  memberRoleIds,
  level,
  roles,
  discordImpl = discord,
}) {
  const targetName = rankForLevel(level)?.name ?? null;
  for (const role of roles) {
    const hasRole = memberRoleIds.includes(role.id);
    const isTarget = targetName != null &&
      normalizeRoleName(role.name) === normalizeRoleName(targetName);
    if (hasRole && !isTarget) {
      await discordImpl(
        "DELETE",
        `/guilds/${guildId}/members/${userId}/roles/${role.id}`,
        null,
      );
    }
    if (!hasRole && isTarget) {
      await discordImpl(
        "PUT",
        `/guilds/${guildId}/members/${userId}/roles/${role.id}`,
        null,
      );
    }
  }
}

export async function getStats(fetchImpl = fetch) {
  const explorer = await json(EXPLORER_STATUS, undefined, fetchImpl);
  return {
    checkedAt: new Date(),
    explorer,
  };
}

export function formatStatsMessage(stats) {
  const explorer = stats.explorer;
  const retargetBlocks = explorer.blocks_to_retarget ?? null;
  const nextRetargetHeight = explorer.next_retarget_height ?? null;
  const payoutWindows = Array.isArray(explorer.payout_address_windows) ? explorer.payout_address_windows : [];
  const window100 = payoutWindows.find((window) => Number(window.requested_blocks) === 100);
  const sourceWindows = Array.isArray(explorer.block_source_windows) ? explorer.block_source_windows : [];
  const sourceWindow100 = sourceWindows.find((window) => Number(window.requested_blocks) === 100);
  const lines = [
    MESSAGE_MARKER,
    "",
    `Network height / peers: **${Number(explorer.height).toLocaleString()} / ${Number(explorer.peers).toLocaleString()}**`,
    `Estimated network hashrate: **${formatHashrate(explorer.estimated_network_hashrate_hps)}**`,
    `Difficulty: **${formatNumber(explorer.difficulty, 2)}**`,
    `Target / avg this window: **${formatDuration(explorer.target_block_seconds)} / ${formatDuration(explorer.epoch_average_block_seconds)}**`,
    `Retarget: **${retargetBlocks == null ? "unavailable" : Number(retargetBlocks).toLocaleString() + " blocks"}**${nextRetargetHeight == null ? "" : `, height **${Number(nextRetargetHeight).toLocaleString()}**`}`,
    `Est. next difficulty: **${formatNumber(explorer.estimated_next_difficulty, 2)}**`,
  ];

  if (sourceWindow100) {
    lines.push(
      `Top solo payout address, last 100 blocks: **${formatNumber(sourceWindow100.top_solo_share_percent, 1)}%** (${Number(sourceWindow100.top_solo_payout_blocks).toLocaleString()} of ${Number(sourceWindow100.observed_blocks).toLocaleString()})`,
      `Distributed/multi-output blocks, last 100: **${Number(sourceWindow100.distributed_blocks).toLocaleString()}**`,
    );
  } else if (window100) {
    lines.push(
      `Top payout address, last 100 blocks: **${formatNumber(window100.top_share_percent, 1)}%** (${Number(window100.top_payout_blocks).toLocaleString()} of ${Number(window100.observed_blocks).toLocaleString()}; ${Number(window100.distinct_payout_addresses).toLocaleString()} payout address${Number(window100.distinct_payout_addresses) === 1 ? "" : "es"})`,
    );
  }

  const advertisedLag = Number(explorer.advertised_peer_height_lag);
  if (Number.isFinite(advertisedLag) && advertisedLag > 0) {
    lines.push(
      `Highest advertised peer tip: **${formatInteger(explorer.highest_advertised_peer_height)}** (local node ${formatInteger(advertisedLag)} block${advertisedLag === 1 ? "" : "s"} behind)`,
    );
  }

  lines.push(
    "",
    "Solo concentration counts single-output blocks; distributed blocks are separate. Peer tips are untrusted health signals.",
    `Explorer: https://explorer.btc09.org | Discord: ${DISCORD_INVITE}`,
    `Updated: <t:${Math.floor(stats.checkedAt.getTime() / 1000)}:R>`,
  );
  return lines.join("\n");
}

export function formatLiveStatChannelNames(stats) {
  const explorer = stats.explorer;
  return [
    `🧱 Height: ${formatInteger(explorer.height)}`,
    `⚡ Hashrate: ${formatHashrate(explorer.estimated_network_hashrate_hps)}`,
    `⛏ Difficulty: ${formatNumber(explorer.difficulty, 2)}`,
    `🌐 Peers: ${formatInteger(explorer.peers)}`,
  ];
}

function permissionOverwritesMatch(actual, expected) {
  const normalize = (overwrites) => overwrites
    .map((overwrite) => ({
      id: String(overwrite.id),
      type: Number(overwrite.type),
      allow: String(overwrite.allow ?? "0"),
      deny: String(overwrite.deny ?? "0"),
    }))
    .sort((left, right) =>
      left.id.localeCompare(right.id) || left.type - right.type,
    );
  return JSON.stringify(normalize(actual)) === JSON.stringify(normalize(expected));
}

export async function syncLiveStatChannels(
  stats,
  {
    guildId = process.env.DISCORD_GUILD_ID,
    clientId = process.env.DISCORD_CLIENT_ID,
    discordImpl = discord,
  } = {},
) {
  if (!guildId) throw new Error("Missing required environment variable: DISCORD_GUILD_ID");
  const channels = await discordImpl("GET", `/guilds/${guildId}/channels`);
  const categoryPermissions = [
    { id: guildId, type: 0, allow: "0", deny: CONNECT_PERMISSION },
  ];
  if (clientId) {
    categoryPermissions.push({
      id: clientId,
      type: 1,
      allow: CONNECT_PERMISSION,
      deny: "0",
    });
  }
  let category = channels.find(
    (channel) => channel.type === CHANNEL_TYPE_CATEGORY && channel.name === LIVE_STATS_CATEGORY,
  );
  if (!category) {
    category = await discordImpl("POST", `/guilds/${guildId}/channels`, {
      name: LIVE_STATS_CATEGORY,
      type: CHANNEL_TYPE_CATEGORY,
      position: 0,
      permission_overwrites: categoryPermissions,
    });
    channels.push(category);
  } else {
    const everybody = (category.permission_overwrites ?? []).find(
      (overwrite) => overwrite.id === guildId && overwrite.type === 0,
    );
    const connectDenied = everybody &&
      (BigInt(everybody.deny ?? "0") & BigInt(CONNECT_PERMISSION)) !== 0n;
    const botOverwrite = clientId
      ? (category.permission_overwrites ?? []).find(
        (overwrite) => overwrite.id === clientId && overwrite.type === 1,
      )
      : null;
    const botCanConnect = !clientId || (
      botOverwrite &&
      (BigInt(botOverwrite.allow ?? "0") & BigInt(CONNECT_PERMISSION)) !== 0n
    );
    if (!connectDenied || !botCanConnect) {
      category = await discordImpl("PATCH", `/channels/${category.id}`, {
        permission_overwrites: categoryPermissions,
      });
    }
  }

  const categories = channels
    .map((channel, index) => ({
      channel: channel.id === category.id ? category : channel,
      index,
    }))
    .filter(({ channel }) => channel.type === CHANNEL_TYPE_CATEGORY)
    .sort(
      (left, right) =>
        Number(left.channel.position) - Number(right.channel.position) ||
        left.index - right.index,
    )
    .map(({ channel }) => channel);
  if (Number(category.position) !== 0 || categories[0]?.id !== category.id) {
    const orderedCategories = [
      category,
      ...categories.filter((existing) => existing.id !== category.id),
    ];
    await discordImpl(
      "PATCH",
      `/guilds/${guildId}/channels`,
      orderedCategories.map((existing, position) => ({
        id: existing.id,
        position,
      })),
    );
  }

  const names = formatLiveStatChannelNames(stats);
  const definitions = [
    { marker: "Height:", name: names[0] },
    { marker: "Hashrate:", name: names[1] },
    { marker: "Difficulty:", name: names[2] },
    { marker: "Peers:", name: names[3] },
  ];
  for (const [position, definition] of definitions.entries()) {
    const existing = channels.find(
      (channel) => channel.type === CHANNEL_TYPE_VOICE && channel.name.includes(definition.marker),
    );
    if (!existing) {
      const created = await discordImpl("POST", `/guilds/${guildId}/channels`, {
        name: definition.name,
        type: CHANNEL_TYPE_VOICE,
        parent_id: category.id,
        position,
        permission_overwrites: categoryPermissions,
      });
      channels.push(created);
      continue;
    }
    const permissionsMatch = !Array.isArray(existing.permission_overwrites) ||
      permissionOverwritesMatch(existing.permission_overwrites, categoryPermissions);
    if (
      existing.name !== definition.name ||
      existing.parent_id !== category.id ||
      !permissionsMatch
    ) {
      await discordImpl("PATCH", `/channels/${existing.id}`, {
        name: definition.name,
        parent_id: category.id,
        position,
        permission_overwrites: categoryPermissions,
      });
    }
  }
}

async function refreshLiveStatChannels() {
  const stats = await getStats();
  await syncLiveStatChannels(stats);
  console.log(`Updated ${LIVE_STATS_CATEGORY} channels.`);
}

export async function refreshAllLiveStats({
  channelRefreshImpl = refreshLiveStatChannels,
  messageRefreshImpl = postOrUpdateStatsMessage,
  logger = console,
} = {}) {
  try {
    await channelRefreshImpl();
  } catch (error) {
    logger.error("Live stats channel update failed:", error.message || error);
  }

  try {
    await messageRefreshImpl();
  } catch (error) {
    logger.error("Live stats message update failed:", error.message || error);
  }
}

export function startLiveStatsUpdater({
  refreshImpl = refreshAllLiveStats,
  setIntervalImpl = setInterval,
  logger = console,
} = {}) {
  const run = async () => {
    try {
      await refreshImpl();
    } catch (error) {
      logger.error("Live stats channel update failed:", error.message || error);
    }
  };
  void run();
  return setIntervalImpl(run, LIVE_STATS_REFRESH_MS);
}

async function json(url, options, fetchImpl = fetch) {
  const response = await fetchImpl(url, options);
  if (!response.ok) {
    const text = await response.text().catch(() => "");
    throw new Error(`${url} failed with ${response.status}: ${text}`);
  }
  return response.json();
}

async function discord(method, path, body, options = {}, attempt = 0) {
  const headers = {
    "Content-Type": "application/json",
  };
  if (options.auth !== false) {
    headers.Authorization = `Bot ${process.env.DISCORD_BOT_TOKEN}`;
  }

  const response = await fetch(`${API_BASE}${path}`, {
    method,
    headers,
    body: body == null ? undefined : JSON.stringify(body),
  });

  if (response.status === 429) {
    const rateLimit = await response.json().catch(() => ({}));
    const retryAfterMs = Math.ceil(Number(rateLimit.retry_after ?? 1) * 1000) + 250;
    if (attempt > 5) {
      const error = new Error(`Discord rate limit did not clear for ${method} ${redactDiscordPath(path)}`);
      error.status = 429;
      throw error;
    }
    await sleep(retryAfterMs);
    return discord(method, path, body, options, attempt + 1);
  }

  if (!response.ok) {
    const text = await response.text();
    const error = new Error(`${method} ${redactDiscordPath(path)} failed with ${response.status}: ${text}`);
    error.status = response.status;
    throw error;
  }

  if (response.status === 204) return null;
  return response.json();
}

function findStatsChannel(channels, wanted) {
  const normalized = normalizeChannelName(wanted);
  return channels.find((channel) => channel.id === wanted) ??
    channels.find((channel) => normalizeChannelName(channel.name) === normalized) ??
    channels.find((channel) => normalizeChannelName(channel.name).includes(normalized));
}

function normalizeChannelName(name) {
  return String(name)
    .toLowerCase()
    .replace(/^[^\p{L}\p{N}]+/u, "")
    .replace(/[^a-z0-9-]/g, "");
}

function normalizeRoleName(name) {
  return String(name)
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, "");
}

function formatHashrate(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "-";
  if (n >= 1_000_000) return `${formatNumber(n / 1_000_000, 2)} MH/s`;
  if (n >= 1_000) return `${formatNumber(n / 1_000, 2)} KH/s`;
  return `${formatNumber(n, 2)} H/s`;
}

function formatInteger(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "-";
  return Math.trunc(n).toLocaleString();
}

function formatDuration(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) return "-";
  if (n >= 3600) return `${formatNumber(n / 3600, 1)}h`;
  if (n >= 60) return `${formatNumber(n / 60, 1)}m`;
  return `${formatNumber(n, 0)}s`;
}

function formatNumber(value, digits) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "-";
  return n.toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

function requireDiscordEnv({ terminal = false } = {}) {
  const missing = ["DISCORD_CLIENT_ID", "DISCORD_GUILD_ID", "DISCORD_BOT_TOKEN"]
    .filter((key) => !process.env[key]);
  if (missing.length) {
    const message = `Missing required environment variables: ${missing.join(", ")}`;
    throw terminal ? terminalStartupError(message) : new Error(message);
  }
}

function terminalStartupError(message) {
  const error = new Error(message);
  error.terminal = true;
  error.exitCode = 0;
  return error;
}

function loadLocalEnv() {
  const envPath = join(scriptDir, ".env");
  if (!existsSync(envPath)) return;

  const text = readFileSync(envPath, "utf8");
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith("#")) continue;
    const match = line.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/);
    if (!match) continue;
    const [, key, rawValue] = match;
    if (process.env[key] != null) continue;
    process.env[key] = rawValue.replace(/^["']|["']$/g, "");
  }
}

function xpStateFile() {
  if (process.env.DISCORD_XP_STATE_FILE) return process.env.DISCORD_XP_STATE_FILE;
  if (process.platform === "win32") return join(scriptDir, ".state", "xp.json");
  return "/var/lib/btc09-discord/xp.json";
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function usage() {
  console.log(`
Usage:
  node tools/discord/stats-bot.mjs
  node tools/discord/stats-bot.mjs --register-commands
  node tools/discord/stats-bot.mjs --post
  node tools/discord/stats-bot.mjs --post --channel=pools-and-nodes
  node tools/discord/stats-bot.mjs --watch

Modes:
  no args              Print current stats locally.
  --register-commands  Register the guild /stats, /rank, /leaderboard, /wallet, and /mine commands.
  --post               Post or update one stats message in Discord.
  --watch              Keep a gateway connection open for role buttons.

Environment:
  DISCORD_CLIENT_ID
  DISCORD_GUILD_ID
  DISCORD_BOT_TOKEN
  DISCORD_XP_STATE_FILE (optional)
`);
}
