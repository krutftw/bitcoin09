#!/usr/bin/env node

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const API_BASE = "https://discord.com/api/v10";
const POOL_ID = "09c";
const POOL_BASE = "https://bitcoin09.tutuit.xyz/api/pools/" + POOL_ID;
const EXPLORER_STATUS = "https://explorer.btc09.178.128.105.41.sslip.io/api/status";
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
  process.exitCode = 1;
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

  const registered = await discord("PUT", `/applications/${process.env.DISCORD_CLIENT_ID}/guilds/${process.env.DISCORD_GUILD_ID}/commands`, commands);
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
  requireDiscordEnv();
  if (typeof WebSocket !== "function") {
    throw new Error("This Node runtime does not provide WebSocket. Use Node 22+ or install a websocket client.");
  }

  const gateway = await discord("GET", "/gateway/bot");
  const url = gateway.url + "/?v=10&encoding=json";
  console.log("Connecting Discord gateway for /stats...");

  let sequence = null;
  let heartbeatTimer = null;
  const socket = new WebSocket(url);

  socket.addEventListener("open", () => {
    console.log("Gateway socket open.");
  });

  socket.addEventListener("message", async (event) => {
    let packet;
    try {
      packet = JSON.parse(event.data);
    } catch {
      return;
    }

    if (packet.s != null) sequence = packet.s;

    if (packet.op === 10) {
      heartbeatTimer = setInterval(() => {
        socket.send(JSON.stringify({ op: 1, d: sequence }));
      }, packet.d.heartbeat_interval);

      socket.send(JSON.stringify({
        op: 2,
        d: {
          token: process.env.DISCORD_BOT_TOKEN,
          intents: 1,
          properties: {
            os: process.platform,
            browser: "bitcoin09-stats-bot",
            device: "bitcoin09-stats-bot",
          },
        },
      }));
      return;
    }

    if (packet.op === 1) {
      socket.send(JSON.stringify({ op: 1, d: sequence }));
      return;
    }

    if (packet.op === 7 || packet.op === 9) {
      console.error("Discord gateway requested reconnect; restart stats-bot.mjs --watch.");
      socket.close();
      return;
    }

    if (packet.t === "READY") {
      console.log(`Ready as ${packet.d.user.username}#${packet.d.user.discriminator ?? "0"}.`);
      return;
    }

    if (packet.t === "INTERACTION_CREATE") {
      await handleInteraction(packet.d).catch((error) => {
        console.error("Interaction failed:", error.message || error);
      });
    }
  });

  socket.addEventListener("close", (event) => {
    if (heartbeatTimer) clearInterval(heartbeatTimer);
    console.log(`Gateway closed: ${event.code} ${event.reason || ""}`.trim());
    process.exitCode = 1;
  });

  socket.addEventListener("error", (event) => {
    console.error("Gateway error:", event.message || event.error || event);
    process.exitCode = 1;
  });
}

async function handleInteraction(interaction) {
  if (interaction.type === 2 && interaction.data?.name === "stats") {
    await handleStatsInteraction(interaction);
    return;
  }

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

  return [
    MESSAGE_MARKER,
    "",
    `Active pool miner addresses: **${activeMinerCount.toLocaleString()}**`,
    `Pool hashrate: **${formatHashrate(poolStats.poolHashrate)}**`,
    `Pool-reported height: **${Number(network.blockHeight ?? 0).toLocaleString()}**`,
    `Explorer height / peers: **${explorerHeight} / ${explorerPeers}**`,
    `Difficulty: **${formatNumber(network.networkDifficulty, 2)}**`,
    `Pool blocks found: **${Number(pool.blocksFound ?? 0).toLocaleString()}**`,
    `Pool paid: **${formatNumber(pool.totalPaid, 4)} 09C**`,
    `Recent block winner addresses: **${stats.uniqueBlockMiners.length.toLocaleString()}**`,
    `Recent payout addresses: **${stats.uniquePaymentAddresses.length.toLocaleString()}**`,
    "",
    "Top active pool addresses:",
    topMiners || "No active pool miners reported.",
    "",
    "Miner count means public-pool payout addresses, not guaranteed unique people.",
    `Pool: https://bitcoin09.tutuit.xyz | Explorer: https://explorer.btc09.178.128.105.41.sslip.io | Discord: ${DISCORD_INVITE}`,
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
    if (attempt > 5) throw new Error(`Discord rate limit did not clear for ${method} ${path}`);
    await sleep(retryAfterMs);
    return discord(method, path, body, options, attempt + 1);
  }

  if (!response.ok) {
    const text = await response.text();
    throw new Error(`${method} ${path} failed with ${response.status}: ${text}`);
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

function formatNumber(value, digits) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "-";
  return n.toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

function requireDiscordEnv() {
  const missing = ["DISCORD_CLIENT_ID", "DISCORD_GUILD_ID", "DISCORD_BOT_TOKEN"]
    .filter((key) => !process.env[key]);
  if (missing.length) {
    throw new Error(`Missing required environment variables: ${missing.join(", ")}`);
  }
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
  --watch              Keep a gateway connection open and answer /stats plus role buttons.

Environment:
  DISCORD_CLIENT_ID
  DISCORD_GUILD_ID
  DISCORD_BOT_TOKEN
`);
}
