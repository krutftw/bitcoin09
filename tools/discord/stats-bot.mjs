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
import { SUPPORTER_TIERS } from "../support/supporter-tiers.mjs";

const API_BASE = "https://discord.com/api/v10";
const EXPLORER_STATUS = "https://explorer.btc09.org/api/status";
const EXCHANGE_STATUS_URL = "https://btc09.org/exchanges.json";
const FUNDING_STATUS_URL = "https://btc09.org/api/support/v1/status";
const DISCORD_INVITE = "https://discord.gg/fUuGzwRTzP";
const MESSAGE_MARKER = "Bitcoin 09 live mining stats";
const EXCHANGE_MESSAGE_MARKER = "Bitcoin 09 exchange status";
const DEFAULT_STATS_CHANNEL = "pools-and-nodes";
const DEFAULT_EXCHANGE_STATUS_CHANNEL = "exchange-status";
const SUPPORT_CLAIM_URL = "http://127.0.0.1:8032/internal/support/v1/claims";
const SUPPORTER_UPDATES_CHANNEL = "💛-supporter-updates";
const SUPPORTER_UPDATES_TOPIC = "Short BTC09 build notes, test releases, and supporter polls from the project bot.";
const SUPPORTER_UPDATES_MESSAGE_MARKER = "BTC09 supporter updates";
const SUPPORTER_LAB_CHANNEL = "💛-supporter-lab";
const SUPPORTER_LAB_TOPIC = "Early BTC09 builds, short feature polls, and practical testing for confirmed project backers.";
const SUPPORTER_LAB_MESSAGE_MARKER = "BTC09 supporter lab";
const LIVE_STATS_CATEGORY = "📊 LIVE STATS";
const LIVE_STATS_REFRESH_MS = 600_000;
const CHANNEL_TYPE_VOICE = 2;
const CHANNEL_TYPE_TEXT = 0;
const CHANNEL_TYPE_CATEGORY = 4;
const CONNECT_PERMISSION = "1048576";
const VIEW_CHANNEL_PERMISSION = "1024";
const SEND_MESSAGES_PERMISSION = "2048";
const SUPPORTER_READ_ONLY_PERMISSIONS = "66560";
const SUPPORTER_CHANNEL_PERMISSIONS = "84992";
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

  if (args.has("--post-exchanges")) {
    await postOrUpdateExchangeStatusMessage();
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
    {
      name: "support",
      description: "Claim a supporter role or see the current perks.",
      type: 1,
      options: [
        {
          type: 1,
          name: "claim",
          description: "Claim your role after a BTC09 support payment finishes.",
          options: [
            {
              type: 3,
              name: "code",
              description: "The private claim code shown on the BTC09 support page.",
              required: true,
            },
          ],
        },
        {
          type: 1,
          name: "perks",
          description: "Show supporter role levels and what they include.",
        },
      ],
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

export async function postOrUpdateExchangeStatusMessage() {
  requireDiscordEnv();

  const channels = await discord("GET", `/guilds/${process.env.DISCORD_GUILD_ID}/channels`);
  const wanted = process.env.DISCORD_EXCHANGE_STATUS_CHANNEL ?? DEFAULT_EXCHANGE_STATUS_CHANNEL;
  const channel = findStatsChannel(channels, wanted);
  if (!channel) {
    throw new Error(`Could not find exchange status channel ${wanted}.`);
  }

  const botUser = await discord("GET", "/users/@me");
  const status = await getExchangeStatus();
  const content = formatExchangeStatusMessage(status);
  const recentMessages = await discord("GET", `/channels/${channel.id}/messages?limit=50`);
  const existing = recentMessages.find(
    (message) => message.author?.id === botUser.id && message.content.includes(EXCHANGE_MESSAGE_MARKER),
  );

  if (existing) {
    await discord("PATCH", `/channels/${channel.id}/messages/${existing.id}`, {
      content,
      allowed_mentions: { parse: [] },
    });
    console.log(`Updated exchange status message in #${channel.name}.`);
    return;
  }

  await discord("POST", `/channels/${channel.id}/messages`, {
    content,
    allowed_mentions: { parse: [] },
  });
  console.log(`Posted exchange status message in #${channel.name}.`);
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
  try {
    const supporterInfrastructure = await syncSupporterInfrastructure(process.env.DISCORD_GUILD_ID);
    await postOrUpdateSupporterUpdatesIntro(supporterInfrastructure.updatesChannel);
    await postOrUpdateSupporterLabIntro(supporterInfrastructure.channel);
  } catch (error) {
    console.error("Supporter role setup failed:", error.message || error);
  }
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
	if (action === "support") await handleSupportInteraction(interaction);
	if (action === "role") await handleRoleButtonInteraction(interaction);
}

export function classifyInteraction(interaction) {
	if (interaction?.type === 2 && interaction.data?.name === "stats") return "stats";
	if (interaction?.type === 2 && interaction.data?.name === "rank") return "rank";
	if (interaction?.type === 2 && interaction.data?.name === "leaderboard") return "leaderboard";
	if (interaction?.type === 2 && interaction.data?.name === "wallet") return "wallet";
	if (interaction?.type === 2 && interaction.data?.name === "mine") return "mine";
	if (interaction?.type === 2 && interaction.data?.name === "support") return "support";
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

export function formatSupportPerks() {
  return [
    "**Supporter roles**",
    "US$5: 💛 Supporter - private build updates",
    "US$25: 🤝 Backer - early test builds, polls, and supporter lab",
    "US$100: 🛠 Builder - public/release credit and priority issue triage",
    "US$250: ⭐ Core Supporter - linked credit and a direct feedback thread",
    "",
    "Finished payments add together, so your role moves up automatically. Credits are optional and use the name or project link you choose.",
    "Support: https://btc09.org/support.html",
    "When the payment page says finished, use `/support claim`.",
  ].join("\n");
}

export function formatSupporterUpdatesIntro() {
  return [
    SUPPORTER_UPDATES_MESSAGE_MARKER,
    "",
    "This is the quiet feed for wallet and miner progress. Test builds, short dev notes, and supporter polls will land here first.",
    "",
    "Current releases: https://github.com/krutftw/bitcoin09/releases/latest",
    "Backers can discuss builds and open feedback threads in the supporter lab.",
  ].join("\n");
}

export function formatSupporterLabIntro() {
  return [
    SUPPORTER_LAB_MESSAGE_MARKER,
    "",
    "Use this channel for early wallet and miner builds, quick polls, bug reports, and practical feedback.",
    "",
    "Builders can post an issue here for priority triage and choose a name for project or release credits. Core Supporters can also open a focused feedback thread and add one project link to their credit.",
  ].join("\n");
}

export async function postOrUpdateSupporterUpdatesIntro(channel, { discordImpl = discord } = {}) {
  return postOrUpdateMarkedMessage({
    channel,
    marker: SUPPORTER_UPDATES_MESSAGE_MARKER,
    content: formatSupporterUpdatesIntro(),
    discordImpl,
  });
}

export async function postOrUpdateSupporterLabIntro(channel, { discordImpl = discord } = {}) {
  return postOrUpdateMarkedMessage({
    channel,
    marker: SUPPORTER_LAB_MESSAGE_MARKER,
    content: formatSupporterLabIntro(),
    discordImpl,
  });
}

async function postOrUpdateMarkedMessage({ channel, marker, content, discordImpl }) {
  if (!channel?.id) throw new Error("supporter channel is unavailable");
  const botUser = await discordImpl("GET", "/users/@me");
  const messages = await discordImpl("GET", `/channels/${channel.id}/messages?limit=50`);
  const existing = messages.find((message) =>
    message.author?.id === botUser.id && message.content.includes(marker)
  );
  if (existing) {
    if (existing.content !== content) {
      await discordImpl("PATCH", `/channels/${channel.id}/messages/${existing.id}`, {
        content,
        allowed_mentions: { parse: [] },
      });
    }
    return existing.id;
  }
  const created = await discordImpl("POST", `/channels/${channel.id}/messages`, {
    content,
    allowed_mentions: { parse: [] },
  });
  return created.id;
}

async function handleSupportInteraction(interaction) {
  const subcommand = interaction.data?.options?.[0];
  if (subcommand?.name === "perks") {
    await respondToInteraction(interaction, formatSupportPerks(), true);
    return;
  }
  if (subcommand?.name !== "claim") {
    await respondToInteraction(interaction, "Use `/support perks` or `/support claim`.", true);
    return;
  }

  await discord("POST", `/interactions/${interaction.id}/${interaction.token}/callback`, {
    type: 5,
    data: { flags: 64 },
  }, { auth: false });

  const claimCode = subcommand.options?.find((option) => option.name === "code")?.value;
  const userId = interaction.member?.user?.id ?? interaction.user?.id;
  const guildId = interaction.guild_id ?? process.env.DISCORD_GUILD_ID;
  try {
    const claim = await claimSupportPayment({ claimCode, userId });
    const infrastructure = await syncSupporterInfrastructure(guildId);
    const memberRoleIds = await getMemberRoleIds(guildId, userId, interaction.member);
    await applySupporterRole({
      guildId,
      userId,
      memberRoleIds,
      tierKey: claim.tier.key,
      roles: infrastructure.roles,
    });
    const total = Number(claim.total_confirmed_usd).toLocaleString(undefined, {
      minimumFractionDigits: 0,
      maximumFractionDigits: 2,
    });
    const unlocked = [`<#${infrastructure.updatesChannel.id}>`];
    if (claim.tier.supporter_lab && infrastructure.channel) unlocked.push(`<#${infrastructure.channel.id}>`);
    await editInteraction(
      interaction.token,
      `Done. Your confirmed total is **US$${total}** and your role is **${claim.tier.role_name}**. You now have access to ${unlocked.join(" and ")}.`,
    );
  } catch (error) {
    await editInteraction(interaction.token, supportClaimErrorMessage(error));
  }
}

export async function claimSupportPayment({
  claimCode,
  userId,
  fetchImpl = fetch,
  claimSecret = process.env.BTC09_SUPPORT_CLAIM_SECRET,
  claimUrl = SUPPORT_CLAIM_URL,
} = {}) {
  if (!/^[A-Za-z0-9_-]{32}$/.test(String(claimCode ?? ""))) {
    const error = new Error("claim code is invalid");
    error.status = 400;
    throw error;
  }
  if (!/^\d{15,22}$/.test(String(userId ?? ""))) {
    const error = new Error("Discord user is unavailable");
    error.status = 400;
    throw error;
  }
  if (!claimSecret) {
    const error = new Error("support claims are temporarily unavailable");
    error.status = 503;
    throw error;
  }

  const response = await fetchImpl(claimUrl, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-BTC09-Claim-Secret": claimSecret,
    },
    body: JSON.stringify({
      claim_code: String(claimCode),
      discord_user_id: String(userId),
    }),
    signal: AbortSignal.timeout(15_000),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload?.error || "support claim failed");
    error.status = response.status;
    throw error;
  }
  const tier = SUPPORTER_TIERS.find((candidate) => candidate.key === payload?.tier?.key);
  if (!tier || payload?.tier?.role_name !== tier.roleName) {
    const error = new Error("support service returned an unknown role");
    error.status = 502;
    throw error;
  }
  return payload;
}

export function supportClaimErrorMessage(error) {
  if (error?.message === "payment is not finished yet") {
    return "That payment is not finished yet. Wait for the support page to say finished, then try again.";
  }
  if (error?.message === "claim code has already been used") {
    return "That claim code has already been used by another Discord account.";
  }
  if (error?.status === 400 || error?.status === 404) {
    return "That claim code is not valid. Copy it again from the finished payment on the support page.";
  }
  return "Support claims are temporarily unavailable. Your payment is still recorded; try again shortly.";
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

export async function syncSupporterInfrastructure(
  guildId,
  {
    clientId = process.env.DISCORD_CLIENT_ID,
    discordImpl = discord,
  } = {},
) {
  if (!guildId) throw new Error("Missing required environment variable: DISCORD_GUILD_ID");
  const guildRoles = await discordImpl("GET", `/guilds/${guildId}/roles`);
  const roles = [];
  for (const tier of SUPPORTER_TIERS) {
    let role = guildRoles.find(
      (candidate) => normalizeRoleName(candidate.name) === normalizeRoleName(tier.roleName),
    );
    if (!role) {
      role = await discordImpl("POST", `/guilds/${guildId}/roles`, {
        name: tier.roleName,
        color: tier.color,
        hoist: true,
        mentionable: false,
        permissions: "0",
      });
      guildRoles.push(role);
      roleCache = null;
    } else if (
      Number(role.color) !== tier.color ||
      role.hoist !== true ||
      role.mentionable !== false ||
      String(role.permissions ?? "0") !== "0"
    ) {
      role = await discordImpl("PATCH", `/guilds/${guildId}/roles/${role.id}`, {
        name: tier.roleName,
        color: tier.color,
        hoist: true,
        mentionable: false,
        permissions: "0",
      });
      const index = guildRoles.findIndex((candidate) => candidate.id === role.id);
      if (index !== -1) guildRoles[index] = role;
      roleCache = null;
    }
    roles.push(role);
  }

  const channels = await discordImpl("GET", `/guilds/${guildId}/channels`);
  const communityCategory = channels.find((channel) =>
    channel.type === CHANNEL_TYPE_CATEGORY && normalizeChannelName(channel.name) === "community"
  );
  if (!communityCategory) throw new Error("Could not find the COMMUNITY category");
  const labPermissions = [
    { id: guildId, type: 0, allow: "0", deny: VIEW_CHANNEL_PERMISSION },
    ...roles
      .filter((role) => SUPPORTER_TIERS.find((tier) =>
        normalizeRoleName(tier.roleName) === normalizeRoleName(role.name)
      )?.supporterLab)
      .map((role) => ({
        id: role.id,
        type: 0,
        allow: SUPPORTER_CHANNEL_PERMISSIONS,
        deny: "0",
      })),
  ];
  if (clientId) {
    labPermissions.push({
      id: clientId,
      type: 1,
      allow: SUPPORTER_CHANNEL_PERMISSIONS,
      deny: "0",
    });
  }

  let channel = channels.find((candidate) =>
    candidate.type === CHANNEL_TYPE_TEXT && normalizeChannelName(candidate.name) === "supporter-lab"
  );
  const channelBody = {
    name: SUPPORTER_LAB_CHANNEL,
    type: CHANNEL_TYPE_TEXT,
    parent_id: communityCategory.id,
    position: 3,
    topic: SUPPORTER_LAB_TOPIC,
    permission_overwrites: labPermissions,
  };
  if (!channel) {
    channel = await discordImpl("POST", `/guilds/${guildId}/channels`, channelBody);
  } else if (
    channel.name !== channelBody.name ||
    channel.parent_id !== channelBody.parent_id ||
    Number(channel.position) !== channelBody.position ||
    channel.topic !== channelBody.topic ||
    !permissionOverwritesMatch(channel.permission_overwrites ?? [], labPermissions)
  ) {
    channel = await discordImpl("PATCH", `/channels/${channel.id}`, channelBody);
  }

  const updatesPermissions = [
    { id: guildId, type: 0, allow: "0", deny: VIEW_CHANNEL_PERMISSION },
    ...roles.map((role) => ({
      id: role.id,
      type: 0,
      allow: SUPPORTER_READ_ONLY_PERMISSIONS,
      deny: SEND_MESSAGES_PERMISSION,
    })),
  ];
  if (clientId) {
    updatesPermissions.push({
      id: clientId,
      type: 1,
      allow: SUPPORTER_CHANNEL_PERMISSIONS,
      deny: "0",
    });
  }
  let updatesChannel = channels.find((candidate) =>
    candidate.type === CHANNEL_TYPE_TEXT && normalizeChannelName(candidate.name) === "supporter-updates"
  );
  const updatesBody = {
    name: SUPPORTER_UPDATES_CHANNEL,
    type: CHANNEL_TYPE_TEXT,
    parent_id: communityCategory.id,
    position: 2,
    topic: SUPPORTER_UPDATES_TOPIC,
    permission_overwrites: updatesPermissions,
  };
  if (!updatesChannel) {
    updatesChannel = await discordImpl("POST", `/guilds/${guildId}/channels`, updatesBody);
  } else if (
    updatesChannel.name !== updatesBody.name ||
    updatesChannel.parent_id !== updatesBody.parent_id ||
    Number(updatesChannel.position) !== updatesBody.position ||
    updatesChannel.topic !== updatesBody.topic ||
    !permissionOverwritesMatch(updatesChannel.permission_overwrites ?? [], updatesPermissions)
  ) {
    updatesChannel = await discordImpl("PATCH", `/channels/${updatesChannel.id}`, updatesBody);
  }
  return { roles, channel, updatesChannel };
}

export async function applySupporterRole({
  guildId,
  userId,
  memberRoleIds,
  tierKey,
  roles,
  discordImpl = discord,
}) {
  const tier = SUPPORTER_TIERS.find((candidate) => candidate.key === tierKey);
  if (!tier) throw new Error("unknown supporter tier");
  for (const role of roles) {
    const hasRole = memberRoleIds.includes(role.id);
    const isTarget = normalizeRoleName(role.name) === normalizeRoleName(tier.roleName);
    if (hasRole && !isTarget) {
      await discordImpl("DELETE", `/guilds/${guildId}/members/${userId}/roles/${role.id}`, null);
    }
    if (!hasRole && isTarget) {
      await discordImpl("PUT", `/guilds/${guildId}/members/${userId}/roles/${role.id}`, null);
    }
  }
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

export async function getExchangeStatus(fetchImpl = fetch) {
  const status = await json(EXCHANGE_STATUS_URL, { cache: "no-store" }, fetchImpl);
  try {
    const funding = await json(FUNDING_STATUS_URL, { cache: "no-store" }, fetchImpl);
    return { ...status, funding: { ...(status?.funding ?? {}), ...funding } };
  } catch {
    return status;
  }
}

export function formatExchangeStatusMessage(status) {
  const summary = status?.summary ?? {};
  const funding = status?.funding ?? {};
  const requirements = Number(summary.requirements_needed ?? 0) + Number(summary.engineering_needed ?? 0);
  const statusUpdated = Date.parse(status?.updated_at ?? "");
  const fundingUpdated = Date.parse(funding?.updated_at ?? "");
  const updatedSeconds = Math.floor(Math.max(
    Number.isFinite(statusUpdated) ? statusUpdated : 0,
    Number.isFinite(fundingUpdated) ? fundingUpdated : 0,
  ) / 1000);
  const updated = Number.isFinite(updatedSeconds) && updatedSeconds > 0 ? `<t:${updatedSeconds}:R>` : "recently";

  return [
    EXCHANGE_MESSAGE_MARKER,
    "",
    `Submitted / awaiting: **${formatInteger(summary.awaiting_reply)}**`,
    `Terms requested: **${formatInteger(summary.terms_requested)}**`,
    `Requirements / engineering: **${formatInteger(requirements)}**`,
    `Published paid routes: **${formatInteger(summary.paid_routes_published)}**`,
    "",
    `Phase 1 target: **US$${formatInteger(funding.cash_received_usd)} / US$${formatInteger(funding.cash_target_usd)} cash** + **US$${formatInteger(funding.coin_liquidity_received_usd)} / US$${formatInteger(funding.coin_liquidity_target_usd)} in 09C**`,
    "<https://btc09.org/exchanges.html>",
    `Updated ${updated}.`,
  ].join("\n");
}

export function formatStatsMessage(stats) {
  const explorer = stats.explorer;
  const retargetBlocks = explorer.blocks_to_retarget ?? null;
  const nextRetargetHeight = explorer.next_retarget_height ?? null;
  const difficultyAlgorithm = explorer.difficulty_algorithm ?? "legacy-2016";
  const asertActivationHeight = explorer.asert_activation_height ?? null;
  const asertBlocks = explorer.blocks_to_asert ?? null;
  const asertHalfLife = formatDuration(explorer.asert_half_life_seconds).replace(".0h", "h");
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
    `Target / recent average: **${formatDuration(explorer.target_block_seconds)} / ${formatDuration(explorer.epoch_average_block_seconds)}**`,
  ];

  if (difficultyAlgorithm === "asert") {
    lines.push(
      `Difficulty adjustment: **ASERT active**, ${asertHalfLife} half-life`,
      `Next block difficulty: **${formatNumber(explorer.next_block_difficulty ?? explorer.estimated_next_difficulty, 2)}**`,
    );
  } else {
    lines.push(
      `Retarget: **${retargetBlocks == null ? "unavailable" : Number(retargetBlocks).toLocaleString() + " blocks"}**${nextRetargetHeight == null ? "" : `, height **${Number(nextRetargetHeight).toLocaleString()}**`}`,
    );
    if (asertActivationHeight != null && Number(asertActivationHeight) > 0) {
      lines.push(
        `ASERT: **${Number(asertBlocks).toLocaleString()} blocks**, height **${Number(asertActivationHeight).toLocaleString()}**`,
      );
    }
    lines.push(`Est. next difficulty: **${formatNumber(explorer.estimated_next_difficulty, 2)}**`);
  }

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
  exchangeMessageRefreshImpl = postOrUpdateExchangeStatusMessage,
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

  try {
    await exchangeMessageRefreshImpl();
  } catch (error) {
    logger.error("Exchange status message update failed:", error.message || error);
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
  node tools/discord/stats-bot.mjs --post-exchanges
  node tools/discord/stats-bot.mjs --post --channel=pools-and-nodes
  node tools/discord/stats-bot.mjs --watch

Modes:
  no args              Print current stats locally.
  --register-commands  Register the guild /stats, /rank, /leaderboard, /wallet, /mine, and /support commands.
  --post               Post or update one stats message in Discord.
  --post-exchanges     Post or update one exchange status message in Discord.
  --watch              Keep a gateway connection open for role buttons.

Environment:
  DISCORD_CLIENT_ID
  DISCORD_GUILD_ID
  DISCORD_BOT_TOKEN
  DISCORD_EXCHANGE_STATUS_CHANNEL (optional)
  DISCORD_XP_STATE_FILE (optional)
  BTC09_SUPPORT_CLAIM_SECRET (required for /support claim)
`);
}
