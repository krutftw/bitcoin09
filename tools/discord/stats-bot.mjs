#!/usr/bin/env node

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { redactDiscordPath } from "./discord-api.mjs";
import { DiscordGatewayWatcher, fetchGatewayWithRetry } from "./gateway-watcher.mjs";

const API_BASE = "https://discord.com/api/v10";
const POOL_ID = "09c";
const POOL_BASE = "https://bitcoin09.tutuit.xyz/api/pools/" + POOL_ID;
const EXPLORER_STATUS = "https://explorer.btc09.org/api/status";
const DISCORD_INVITE = "https://discord.gg/fUuGzwRTzP";
const MESSAGE_MARKER = "Bitcoin 09 live mining stats";
const DEFAULT_STATS_CHANNEL = "pools-and-nodes";
const scriptDir = dirname(fileURLToPath(import.meta.url));
const selfAssignableRoles = [
  { key: "miner", label: "Miner", roleName: "⛏ Miner" },
  { key: "node_operator", label: "Node Operator", roleName: "🧱 Node Operator" },
  { key: "updates", label: "Updates", roleName: "🔔 Updates" },
  { key: "contributor", label: "Contributor", roleName: "🤝 Contributor" },
  { key: "tester", label: "Tester", roleName: "🧪 Tester" },
];
let roleCache = null;

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

main().catch((error) => {
  console.error(error.message || error);
  process.exitCode = error.exitCode ?? 1;
});

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

  const commands = [
    {
      name: "stats",
      description: "Show live Bitcoin 09 mining and network stats.",
      type: 1,
    },
  ];

  const registered = [];
  for (const command of commands) {
    registered.push(await discord("POST", `/applications/${process.env.DISCORD_CLIENT_ID}/guilds/${process.env.DISCORD_GUILD_ID}/commands`, command));
  }
  for (const command of registered) {
    console.log(`Registered /${command.name} (${command.id}) in guild ${process.env.DISCORD_GUILD_ID}.`);
  }
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
      if (packet.t !== "INTERACTION_CREATE") return;
      await handleInteraction(packet.d).catch((error) => {
        console.error("Interaction failed:", error.message || error);
      });
    },
    onFatal: (decision) => {
      process.exitCode = decision.exitCode;
    },
  });
  watcher.start();
}

async function handleInteraction(interaction) {
  if (interaction.type === 3 && interaction.data?.custom_id?.startsWith("role:toggle:")) {
    await handleRoleButtonInteraction(interaction);
  }
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

async function getStats() {
  const [poolData, miners, blocks, payments, explorer] = await Promise.all([
    json(POOL_BASE),
    json(`${POOL_BASE}/miners?pageSize=20`),
    json(`${POOL_BASE}/blocks?pageSize=50`),
    json(`${POOL_BASE}/payments?pageSize=50`),
    json(EXPLORER_STATUS).catch(() => null),
  ]);

  const uniqueBlockMiners = unique(blocks.map((block) => block.miner));
  const uniquePaymentAddresses = unique(payments.map((payment) => payment.address));

  return {
    checkedAt: new Date(),
    pool: poolData.pool,
    miners,
    explorer,
    uniqueBlockMiners,
    uniquePaymentAddresses,
  };
}

function formatStatsMessage(stats) {
  const pool = stats.pool;
  const network = pool.networkStats ?? {};
  const poolStats = pool.poolStats ?? {};
  const activeMinerCount = Number(poolStats.connectedMiners ?? stats.miners.length);
  const topMiners = stats.miners
    .slice(0, 5)
    .map((miner, index) => `${index + 1}. \`${miner.miner}\` - ${formatHashrate(miner.hashrate)}`)
    .join("\n");
  const explorerPeers = stats.explorer ? Number(stats.explorer.peers).toLocaleString() : "unavailable";
  const explorerHeight = stats.explorer ? Number(stats.explorer.height).toLocaleString() : "unavailable";
  const retargetBlocks = stats.explorer?.blocks_to_retarget ?? null;
  const nextRetargetHeight = stats.explorer?.next_retarget_height ?? null;

  return [
    MESSAGE_MARKER,
    "",
    `Active pool miner addresses: **${activeMinerCount.toLocaleString()}**`,
    `Pool hashrate: **${formatHashrate(poolStats.poolHashrate)}**`,
    `Pool-reported height: **${Number(network.blockHeight ?? 0).toLocaleString()}**`,
    `Explorer height / peers: **${explorerHeight} / ${explorerPeers}**`,
    `Difficulty: **${formatNumber(network.networkDifficulty, 2)}**`,
    `Target / avg this window: **${formatDuration(stats.explorer?.target_block_seconds)} / ${formatDuration(stats.explorer?.epoch_average_block_seconds)}**`,
    `Retarget: **${retargetBlocks == null ? "unavailable" : Number(retargetBlocks).toLocaleString() + " blocks"}**${nextRetargetHeight == null ? "" : `, height **${Number(nextRetargetHeight).toLocaleString()}**`}`,
    `Est. next difficulty: **${formatNumber(stats.explorer?.estimated_next_difficulty, 2)}**`,
    `Pool blocks found: **${Number(pool.blocksFound ?? 0).toLocaleString()}**`,
    `Pool paid: **${formatNumber(pool.totalPaid, 4)} 09C**`,
    `Recent block winner addresses: **${stats.uniqueBlockMiners.length.toLocaleString()}**`,
    `Recent payout addresses: **${stats.uniquePaymentAddresses.length.toLocaleString()}**`,
    "",
    "Top active pool addresses:",
    topMiners || "No active pool miners reported.",
    "",
    "Difficulty retargets every 2,016 blocks, Bitcoin-style. Miner count means public-pool payout addresses, not guaranteed unique people.",
    `Pool: https://bitcoin09.tutuit.xyz | Explorer: https://explorer.btc09.org | Discord: ${DISCORD_INVITE}`,
    `Updated: <t:${Math.floor(stats.checkedAt.getTime() / 1000)}:R>`,
  ].join("\n");
}

async function json(url, options) {
  const response = await fetch(url, options);
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

function unique(values) {
  return [...new Set(values.filter(Boolean))];
}

function formatHashrate(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "-";
  if (n >= 1_000_000) return `${formatNumber(n / 1_000_000, 2)} MH/s`;
  if (n >= 1_000) return `${formatNumber(n / 1_000, 2)} KH/s`;
  return `${formatNumber(n, 2)} H/s`;
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
  --register-commands  Register the guild /stats command.
  --post               Post or update one stats message in Discord.
  --watch              Keep a gateway connection open for role buttons.

Environment:
  DISCORD_CLIENT_ID
  DISCORD_GUILD_ID
  DISCORD_BOT_TOKEN
`);
}
