#!/usr/bin/env node

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const API_BASE = "https://discord.com/api/v10";
const scriptDir = dirname(fileURLToPath(import.meta.url));

const ChannelType = {
  GuildText: 0,
  GuildVoice: 2,
  GuildCategory: 4,
};

const Permission = {
  CreateInstantInvite: 1n << 0n,
  ManageChannels: 1n << 4n,
  ViewChannel: 1n << 10n,
  SendMessages: 1n << 11n,
  EmbedLinks: 1n << 14n,
  ReadMessageHistory: 1n << 16n,
  ManageRoles: 1n << 28n,
};

const INVITE_PERMISSIONS = (
  Permission.CreateInstantInvite |
  Permission.ManageChannels |
  Permission.ViewChannel |
  Permission.SendMessages |
  Permission.EmbedLinks |
  Permission.ReadMessageHistory |
  Permission.ManageRoles
).toString();

const args = new Set(process.argv.slice(2));
const dryRun = !args.has("--apply");
const seedMessages = args.has("--seed");

loadLocalEnv();

if (args.has("--help")) {
  usage();
  process.exit(0);
}

if (args.has("--invite")) {
  printInviteUrl();
  if (process.argv.slice(2).length === 1) process.exit(0);
}

const token = process.env.DISCORD_BOT_TOKEN;
const guildId = process.env.DISCORD_GUILD_ID;

if (!token || !guildId) {
  usage();
  throw new Error("Set DISCORD_BOT_TOKEN and DISCORD_GUILD_ID before running setup.");
}

const auditReason = encodeURIComponent("Bitcoin 09 Discord server setup");

const ownerDisplayRole = { name: "👑 Owner", aliases: ["Owner"], color: 0xf1c40f, hoist: true, mentionable: false };
const botDisplayRole = { name: "🤖 Bot", aliases: ["Bot"], color: 0x5865f2, hoist: true, mentionable: false };

const desiredRoles = [
  ownerDisplayRole,
  botDisplayRole,
  { name: "⛏ Miner", aliases: ["Miner"], color: 0xf2c94c, hoist: false, mentionable: true },
  { name: "🧱 Node Operator", aliases: ["Node Operator"], color: 0x2d9cdb, hoist: false, mentionable: true },
  { name: "🏊 Pool Operator", aliases: ["Pool Operator"], color: 0x27ae60, hoist: true, mentionable: true },
  { name: "🛠 Developer", aliases: ["Developer"], color: 0x9b51e0, hoist: true, mentionable: true },
  { name: "🛡 Moderator", aliases: ["Moderator"], color: 0xeb5757, hoist: true, mentionable: true },
  { name: "🔔 Updates", aliases: ["📣 Announcements", "Announcements", "Updates"], color: 0xf2994a, hoist: false, mentionable: true },
  { name: "🤝 Contributor", color: 0x00b894, hoist: false, mentionable: true },
  { name: "🧪 Tester", color: 0x56ccf2, hoist: false, mentionable: true },
];

const desiredCategories = [
  { key: "info", name: "📌 INFO", aliases: ["INFO"], position: 0 },
  { key: "community", name: "💬 COMMUNITY", aliases: ["COMMUNITY"], position: 1 },
  { key: "mining", name: "⛏ MINING", aliases: ["MINING"], position: 2 },
  { key: "network", name: "🌐 NETWORK", aliases: ["NETWORK"], position: 3 },
  { key: "development", name: "🛠 DEVELOPMENT", aliases: ["DEVELOPMENT"], position: 4 },
  { key: "voice", name: "🔊 VOICE", aliases: ["Voice Channels"], position: 5 },
];

const desiredChannels = [
  {
    key: "announcements",
    name: "📣-announcements",
    aliases: ["announcements"],
    category: "info",
    position: 0,
    topic: "Release notices, network status, and urgent upgrade notes for Bitcoin 09.",
    lockedForEveryone: true,
  },
  {
    key: "start-here",
    name: "👋-start-here",
    aliases: ["start-here"],
    category: "info",
    position: 1,
    topic: "Quick links, current release, explorer, seed node, and pool information.",
    lockedForEveryone: true,
  },
  {
    key: "rules",
    name: "📜-rules",
    aliases: ["rules"],
    category: "info",
    position: 2,
    topic: "Basic server rules: stay factual, no scams, no impersonation, no spam.",
    lockedForEveryone: true,
  },
  {
    key: "roles",
    name: "🎭-roles",
    aliases: ["roles"],
    category: "info",
    position: 3,
    topic: "Choose public roles with the buttons in this channel. Staff and operator roles are assigned manually.",
  },
  {
    key: "resources",
    name: "🔗-resources",
    aliases: ["resources"],
    category: "info",
    position: 4,
    topic: "Official Bitcoin 09 links, docs, releases, and reference material.",
    lockedForEveryone: true,
  },
  {
    key: "general",
    name: "💬-general",
    aliases: ["general"],
    category: "community",
    position: 0,
    topic: "General Bitcoin 09 discussion.",
  },
  {
    key: "otc-trading",
    name: "💱-otc-trading",
    aliases: ["otc-trading", "trading", "markets", "market"],
    category: "community",
    position: 1,
    topic: "Community OTC buy/sell posts for 09C. No official price, no escrow, no price promises.",
  },
  {
    key: "mining-help",
    name: "⛏-mining-help",
    aliases: ["mining-help"],
    category: "mining",
    position: 0,
    topic: "CPU mining setup, hashrate reports, commands, logs, and troubleshooting.",
  },
  {
    key: "hashrate",
    name: "📈-hashrate",
    aliases: ["hashrate"],
    category: "mining",
    position: 1,
    topic: "Post CPU, miner version, command, hashrate, and stability notes.",
  },
  {
    key: "pools-and-nodes",
    name: "🏊-pools-and-nodes",
    aliases: ["pools-and-nodes"],
    category: "network",
    position: 0,
    topic: "Pool status, seed nodes, peer connectivity, and node announcements.",
  },
  {
    key: "node-operators",
    name: "🧱-node-operators",
    aliases: ["node-operators"],
    category: "network",
    position: 1,
    topic: "Full node operators, peer counts, sync status, and infrastructure notes.",
  },
  {
    key: "dev-log",
    name: "🛠-dev-log",
    aliases: ["dev-log"],
    category: "development",
    position: 0,
    topic: "Development notes, release work, fixes, and planned changes.",
  },
  {
    key: "bug-reports",
    name: "🐞-bug-reports",
    aliases: ["bug-reports"],
    category: "development",
    position: 1,
    topic: "Report reproducible wallet, sync, mining, pool, explorer, or build bugs.",
  },
  {
    key: "ideas",
    name: "💡-suggestions",
    aliases: ["💡-ideas", "ideas", "feature-ideas", "suggestions", "feedback"],
    category: "development",
    position: 2,
    topic: "Suggestions for miner UX, docs, explorer, wallets, pools, listings, and community setup.",
  },
];

const desiredVoiceChannels = [
  {
    key: "lobby",
    name: "🔊-lobby",
    aliases: ["General"],
    category: "voice",
    position: 0,
  },
  {
    key: "mining-room",
    name: "⛏-mining-room",
    aliases: ["mining-room"],
    category: "voice",
    position: 1,
  },
  {
    key: "dev-sync",
    name: "🛠-dev-sync",
    aliases: ["dev-sync"],
    category: "voice",
    position: 2,
  },
];

const rolePickerComponents = [
  {
    type: 1,
    components: [
      { type: 2, style: 2, custom_id: "role:toggle:miner", label: "⛏ Miner" },
      { type: 2, style: 2, custom_id: "role:toggle:node_operator", label: "🧱 Node Operator" },
      { type: 2, style: 2, custom_id: "role:toggle:updates", label: "🔔 Updates" },
      { type: 2, style: 2, custom_id: "role:toggle:contributor", label: "🤝 Contributor" },
      { type: 2, style: 2, custom_id: "role:toggle:tester", label: "🧪 Tester" },
    ],
  },
];

const marketBoardUrl = "https://krutftw.github.io/bitcoin09/markets.html";
const marketDraftUrl = `${marketBoardUrl}#draft`;
const marketRecordsUrl = "https://github.com/krutftw/bitcoin09/issues?q=label%3Aotc-offer+OR+label%3Aotc-completed";
const marketBoardComponents = [
  {
    type: 1,
    components: [
      { type: 2, style: 5, label: "Draft OTC record", url: marketDraftUrl },
      { type: 2, style: 5, label: "OTC board", url: marketBoardUrl },
      { type: 2, style: 5, label: "Public records", url: marketRecordsUrl },
      { type: 2, style: 5, label: "Explorer", url: "http://82.22.32.82:8009" },
    ],
  },
];

const seedPosts = [
  {
    channelKey: "announcements",
    marker: "Bitcoin 09 Discord is live.",
    content: [
      "Bitcoin 09 Discord is live.",
      "",
      "Current release: v0.1.11",
      "Source/releases: https://github.com/krutftw/bitcoin09",
      "Explorer: http://82.22.32.82:8009",
      "Public pool: https://bitcoin09.tutuit.xyz",
      "Discord invite: https://discord.gg/fUuGzwRTzP",
      "Seed: 82.22.32.82:9009",
      "",
      "If you mined on an early build, upgrade before syncing or mining. Older clients can get stuck on stale forks from before the sync and retarget fixes.",
    ].join("\n"),
  },
  {
    channelKey: "start-here",
    marker: "Bitcoin 09 quick links",
    content: [
      "Bitcoin 09 quick links",
      "",
      "- Source/releases: https://github.com/krutftw/bitcoin09",
      "- Explorer: http://82.22.32.82:8009",
      "- Public pool: https://bitcoin09.tutuit.xyz",
      "- Bitcointalk ANN: https://bitcointalk.org/index.php?topic=5587640.0",
      "- Discord: https://discord.gg/fUuGzwRTzP",
      "- Seed node: 82.22.32.82:9009",
      "- OTC board: https://krutftw.github.io/bitcoin09/markets.html",
      "- Roles: click the buttons in #🎭-roles",
      "",
      "09C has no premine, no ICO, no allocation, and the genesis reward is burned/unspendable.",
    ].join("\n"),
  },
  {
    channelKey: "rules",
    marker: "Bitcoin 09 server rules",
    content: [
      "Bitcoin 09 server rules",
      "",
      "1. Stay factual. If you report hashrate, sync state, bugs, or pools, include enough detail to reproduce it.",
      "2. No scams, impersonation, fake listings, paid shilling, or price promises.",
      "3. Keep mining support in ⛏-mining-help and node/pool status in 🏊-pools-and-nodes.",
      "4. Do not DM members for wallets, keys, seed phrases, remote access, or payments.",
    ].join("\n"),
  },
  {
    channelKey: "roles",
    marker: "Bitcoin 09 role picker",
    content: [
      "Bitcoin 09 role picker",
      "",
      "Click a button to add a role. Click it again to remove it.",
      "",
      "Updates means the opt-in ping role for releases, fork warnings, pool/node incidents, and important network notes. It does not give posting permissions.",
      "",
      "Manual roles:",
      "- Owner",
      "- Bot",
      "- Pool Operator",
      "- Developer",
      "- Moderator",
      "",
      "Manual roles are for identity, trust, or permissions. Ask in general if one of those should apply.",
    ].join("\n"),
    components: rolePickerComponents,
  },
  {
    channelKey: "resources",
    marker: "Official Bitcoin 09 resources",
    content: [
      "Official Bitcoin 09 resources",
      "",
      "- GitHub: https://github.com/krutftw/bitcoin09",
      "- Latest release: https://github.com/krutftw/bitcoin09/releases/latest",
      "- Explorer: http://82.22.32.82:8009",
      "- Public pool: https://bitcoin09.tutuit.xyz",
      "- Bitcointalk ANN: https://bitcointalk.org/index.php?topic=5587640.0",
      "- Discord: https://discord.gg/fUuGzwRTzP",
      "- OTC board: https://krutftw.github.io/bitcoin09/markets.html",
      "- Brand kit: https://github.com/krutftw/bitcoin09/blob/master/BRAND.md",
      "- Logo PNG: https://krutftw.github.io/bitcoin09/assets/bitcoin09-ai-logo-512.png",
      "- Social card: https://krutftw.github.io/bitcoin09/assets/bitcoin09-social.png",
    ].join("\n"),
  },
  {
    channelKey: "otc-trading",
    marker: "Bitcoin 09 OTC trading",
    content: [
      "Bitcoin 09 OTC trading",
      "",
      "This is a community buy/sell channel, not an official exchange.",
      "",
      "There is no official 09C price yet. A price only means what a buyer and seller agreed for that trade.",
      "",
      "Use Discord for quick negotiation. Use the OTC board to draft a clean post, copy it here, and open a public GitHub record when the offer or completed trade should be visible.",
      "",
      "Use simple posts:",
      "- WTB 500 09C, paying in BTC/USDT/AUD",
      "- WTS 1000 09C, asking BTC/USDT/AUD",
      "- Completed: 500 09C for $X or X BTC",
      "",
      "The website reads public GitHub records into the board automatically. Do not post private payment account details, wallet files, seed phrases, Discord tokens, or remote access screenshots.",
      "",
      "Sending 09C is done with the native btc09 wallet, not MetaMask or Phantom:",
      "`btc09 send -to ADDRESS -amount 100 -seeds 82.22.32.82:9009`",
      "",
      "Rules:",
      "1. Trade small first.",
      "2. Never share seed phrases, private keys, remote access, or wallet files.",
      "3. Staff will not DM you first and does not provide official escrow.",
      "4. No fake volume, wash trades, pump groups, price promises, or impersonation.",
      "5. Keep completed-trade references factual so the community can see real price discovery.",
    ].join("\n"),
    components: marketBoardComponents,
  },
  {
    channelKey: "mining-help",
    marker: "Mining help format",
    content: [
      "Mining help format",
      "",
      "When asking for mining help, include:",
      "- OS and CPU",
      "- miner command",
      "- hashrate if available",
      "- release version",
      "- relevant logs or error text",
    ].join("\n"),
  },
  {
    channelKey: "hashrate",
    marker: "Hashrate report format",
    content: [
      "Hashrate report format",
      "",
      "Post:",
      "- CPU model",
      "- OS",
      "- miner command",
      "- workers/threads",
      "- average hashrate",
      "- release version",
    ].join("\n"),
  },
  {
    channelKey: "pools-and-nodes",
    marker: "Current public endpoints",
    content: [
      "Current public endpoints",
      "",
      "- Seed: 82.22.32.82:9009",
      "- Explorer: http://82.22.32.82:8009",
      "- Public pool: https://bitcoin09.tutuit.xyz",
      "",
      "Post new pools or reliable nodes here with host, port, fee/payout details, and operator contact.",
    ].join("\n"),
  },
  {
    channelKey: "ideas",
    marker: "Bitcoin 09 suggestions",
    content: [
      "Bitcoin 09 suggestions",
      "",
      "Drop practical ideas here.",
      "",
      "Good posts are simple:",
      "- what is annoying right now",
      "- what you want changed",
      "- what would make mining, running a node, trading, or explaining 09C easier",
      "",
      "Small fixes are useful. If it is broken or reproducible, use #🐞-bug-reports instead.",
    ].join("\n"),
  },
];

main().catch((error) => {
  console.error(error.message || error);
  process.exitCode = 1;
});

async function main() {
  console.log(`Bitcoin 09 Discord setup (${dryRun ? "dry run" : "apply"})`);
  if (seedMessages && dryRun) {
    console.log("Note: --seed was supplied, but dry run mode will not send messages.");
  }

  const botUser = await discord("GET", "/users/@me");
  const guild = await discord("GET", `/guilds/${guildId}`);
  let channels = await discord("GET", `/guilds/${guildId}/channels`);
  let roles = await discord("GET", `/guilds/${guildId}/roles`);

  console.log(`Bot: ${botUser.username}#${botUser.discriminator ?? "0"} (${botUser.id})`);
  console.log(`Guild: ${guild.name} (${guild.id})`);

  const roleMap = mapByLowerName(roles);
  for (const role of desiredRoles) {
    await ensureRole(roleMap, role);
  }

  const ownerRole = findByDesiredName(roleMap, ownerDisplayRole);
  const botRole = findByDesiredName(roleMap, botDisplayRole);
  if (ownerRole && guild.owner_id) {
    await ensureMemberRole(guild.owner_id, ownerRole, "guild owner");
  }
  if (botRole) {
    await ensureMemberRole(botUser.id, botRole, "bot user");
  }

  channels = await discord("GET", `/guilds/${guildId}/channels`);
  const categoryMap = mapByLowerName(channels.filter((channel) => channel.type === ChannelType.GuildCategory));
  const ensuredCategories = new Map();
  for (const category of desiredCategories) {
    const ensured = await ensureCategory(categoryMap, category);
    ensuredCategories.set(category.key, ensured);
  }

  channels = dryRun ? channels : await discord("GET", `/guilds/${guildId}/channels`);
  const refreshedCategoryMap = dryRun
    ? categoryMap
    : indexDesiredItems(channels.filter((channel) => channel.type === ChannelType.GuildCategory), desiredCategories);
  const textChannelMap = mapByLowerName(channels.filter((channel) => channel.type === ChannelType.GuildText));
  const ensuredChannels = new Map();

  for (const channel of desiredChannels) {
    const ensured = await ensureTextChannel(textChannelMap, refreshedCategoryMap, channel);
    ensuredChannels.set(channel.key, ensured);
  }

  channels = dryRun ? channels : await discord("GET", `/guilds/${guildId}/channels`);
  const refreshedVoiceCategoryMap = dryRun
    ? refreshedCategoryMap
    : indexDesiredItems(channels.filter((channel) => channel.type === ChannelType.GuildCategory), desiredCategories);
  const voiceChannelMap = mapByLowerName(channels.filter((channel) => channel.type === ChannelType.GuildVoice));

  for (const channel of desiredVoiceChannels) {
    await ensureVoiceChannel(voiceChannelMap, refreshedVoiceCategoryMap, channel);
  }

  for (const channel of desiredChannels.filter((item) => item.lockedForEveryone)) {
    const ensured = ensuredChannels.get(channel.key);
    if (ensured) {
      await ensureEveryoneCannotSend(ensured);
      await ensureBotCanSend(botUser.id, ensured);
    }
  }

  if (seedMessages && !dryRun) {
    for (const post of seedPosts) {
      const channel = ensuredChannels.get(post.channelKey);
      if (!channel) throw new Error(`Cannot seed ${post.channelKey}; channel was not found.`);
      await ensureSeedMessage(botUser.id, channel, post);
    }
  } else if (seedMessages) {
    for (const post of seedPosts) {
      action("seed message", `${post.channelKey}: ${post.marker}`);
    }
  }

  channels = dryRun ? channels : await discord("GET", `/guilds/${guildId}/channels`);
  await deleteEmptyDefaultCategory(channels, "Text Channels");

  console.log("Done.");
}

async function ensureRole(roleMap, desired) {
  const existing = findByDesiredName(roleMap, desired);
  const payload = {
    name: desired.name,
    color: desired.color,
    hoist: desired.hoist,
    mentionable: desired.mentionable,
    permissions: "0",
  };

  if (!existing) {
    action("create role", desired.name);
    if (!dryRun) {
      const created = await discord("POST", `/guilds/${guildId}/roles`, payload);
      setDesiredItem(roleMap, created, desired);
    }
    return;
  }

  if (existing.managed) {
    action("skip managed role", desired.name);
    return;
  }

  const needsUpdate =
    existing.name !== desired.name ||
    existing.color !== desired.color ||
    existing.hoist !== desired.hoist ||
    existing.mentionable !== desired.mentionable;

  if (needsUpdate) {
    action("update role", desired.name);
    if (!dryRun) {
      const updated = await discord("PATCH", `/guilds/${guildId}/roles/${existing.id}`, payload);
      setDesiredItem(roleMap, updated, desired);
    }
  } else {
    action("role exists", desired.name);
  }
}

async function ensureCategory(categoryMap, desired) {
  const existing = findByDesiredName(categoryMap, desired);
  if (existing) {
    const patch = {};
    if (existing.name !== desired.name) patch.name = desired.name;
    if (existing.position !== desired.position) patch.position = desired.position;

    if (Object.keys(patch).length > 0) {
      action("update category", desired.name);
      if (dryRun) {
        const updated = { ...existing, ...patch };
        setDesiredItem(categoryMap, updated, desired);
        return updated;
      }
      const updated = await discord("PATCH", `/channels/${existing.id}`, patch);
      setDesiredItem(categoryMap, updated, desired);
      return updated;
    }

    action("category exists", desired.name);
    setDesiredItem(categoryMap, existing, desired);
    return existing;
  }

  action("create category", desired.name);
  if (dryRun) {
    const dryCategory = { id: `dry-${desired.name}`, name: desired.name, type: ChannelType.GuildCategory };
    setDesiredItem(categoryMap, dryCategory, desired);
    return dryCategory;
  }

  const created = await discord("POST", `/guilds/${guildId}/channels`, {
    name: desired.name,
    type: ChannelType.GuildCategory,
    position: desired.position,
  });
  setDesiredItem(categoryMap, created, desired);
  return created;
}

async function ensureTextChannel(textChannelMap, categoryMap, desired) {
  const category = categoryMap.get(desired.category);
  if (!category) throw new Error(`Missing category ${desired.category} for #${desired.name}`);

  const existing = findByDesiredName(textChannelMap, desired);
  const payload = {
    name: desired.name,
    type: ChannelType.GuildText,
    parent_id: category.id,
    topic: desired.topic,
    position: desired.position,
  };

  if (!existing) {
    action("create text channel", `#${desired.name} under ${desired.category}`);
    if (dryRun) return { id: `dry-${desired.name}`, name: desired.name, type: ChannelType.GuildText, position: desired.position, permission_overwrites: [] };
    const created = await discord("POST", `/guilds/${guildId}/channels`, payload);
    setDesiredItem(textChannelMap, created, desired);
    return created;
  }

  const patch = {};
  if (existing.name !== desired.name) patch.name = desired.name;
  if (existing.parent_id !== category.id) patch.parent_id = category.id;
  if ((existing.topic ?? "") !== desired.topic) patch.topic = desired.topic;
  if (existing.position !== desired.position) patch.position = desired.position;

  if (Object.keys(patch).length > 0) {
    action("update text channel", `#${desired.name}`);
    if (!dryRun) {
      const updated = await discord("PATCH", `/channels/${existing.id}`, patch);
      setDesiredItem(textChannelMap, updated, desired);
      return updated;
    }
    return { ...existing, ...patch };
  } else {
    action("text channel exists", `#${desired.name}`);
  }

  return existing;
}

async function ensureVoiceChannel(voiceChannelMap, categoryMap, desired) {
  const category = categoryMap.get(desired.category);
  if (!category) throw new Error(`Missing category ${desired.category} for voice channel ${desired.name}`);

  const existing = findByDesiredName(voiceChannelMap, desired);
  const payload = {
    name: desired.name,
    type: ChannelType.GuildVoice,
    parent_id: category.id,
    position: desired.position,
  };

  if (!existing) {
    action("create voice channel", `${desired.name} under ${desired.category}`);
    if (dryRun) return { id: `dry-${desired.name}`, name: desired.name, type: ChannelType.GuildVoice, position: desired.position };
    const created = await discord("POST", `/guilds/${guildId}/channels`, payload);
    setDesiredItem(voiceChannelMap, created, desired);
    return created;
  }

  const patch = {};
  if (existing.name !== desired.name) patch.name = desired.name;
  if (existing.parent_id !== category.id) patch.parent_id = category.id;
  if (existing.position !== desired.position) patch.position = desired.position;

  if (Object.keys(patch).length > 0) {
    action("update voice channel", desired.name);
    if (!dryRun) {
      const updated = await discord("PATCH", `/channels/${existing.id}`, patch);
      setDesiredItem(voiceChannelMap, updated, desired);
      return updated;
    }
    return { ...existing, ...patch };
  }

  action("voice channel exists", desired.name);
  return existing;
}

async function deleteEmptyDefaultCategory(channels, name) {
  const category = channels.find(
    (channel) => channel.type === ChannelType.GuildCategory && normalizeName(channel.name) === normalizeName(name),
  );
  if (!category) return;

  const childCount = channels.filter((channel) => channel.parent_id === category.id).length;
  if (childCount > 0) {
    action("keep default category", `${name}: ${childCount} child channel(s)`);
    return;
  }

  action("delete empty default category", name);
  if (dryRun) return;
  await discord("DELETE", `/channels/${category.id}`);
}

async function ensureEveryoneCannotSend(channel) {
  const everyoneRoleId = guildId;
  const existingOverwrite = channel.permission_overwrites?.find(
    (overwrite) => overwrite.id === everyoneRoleId && overwrite.type === 0,
  );
  const allow = (BigInt(existingOverwrite?.allow ?? "0") & ~Permission.SendMessages).toString();
  const deny = (BigInt(existingOverwrite?.deny ?? "0") | Permission.SendMessages).toString();

  if (existingOverwrite?.allow === allow && existingOverwrite?.deny === deny) {
    action("permissions exist", `#${channel.name}: @everyone cannot send`);
    return;
  }

  action("lock channel", `#${channel.name}: deny @everyone Send Messages`);
  if (dryRun) return;

  await discord("PUT", `/channels/${channel.id}/permissions/${everyoneRoleId}`, {
    type: 0,
    allow,
    deny,
  });
}

async function ensureBotCanSend(botUserId, channel) {
  const existingOverwrite = channel.permission_overwrites?.find(
    (overwrite) => overwrite.id === botUserId && overwrite.type === 1,
  );
  const neededAllow =
    Permission.ViewChannel |
    Permission.SendMessages |
    Permission.EmbedLinks |
    Permission.ReadMessageHistory;
  const allow = (BigInt(existingOverwrite?.allow ?? "0") | neededAllow).toString();
  const deny = (BigInt(existingOverwrite?.deny ?? "0") & ~neededAllow).toString();

  if (existingOverwrite?.allow === allow && existingOverwrite?.deny === deny) {
    action("permissions exist", `#${channel.name}: bot can send`);
    return;
  }

  action("allow bot", `#${channel.name}: send starter messages`);
  if (dryRun) return;

  await discord("PUT", `/channels/${channel.id}/permissions/${botUserId}`, {
    type: 1,
    allow,
    deny,
  });
}

async function ensureSeedMessage(botUserId, channel, post) {
  const recentMessages = await discord("GET", `/channels/${channel.id}/messages?limit=50`);
  const existingMessage = recentMessages.find(
    (message) => message.author?.id === botUserId && message.content.includes(post.marker),
  );
  const payload = {
    content: post.content,
    components: post.components ?? [],
    allowed_mentions: { parse: [] },
  };

  if (existingMessage) {
    if (existingMessage.content === post.content && sameComponents(existingMessage.components ?? [], post.components ?? [])) {
      action("seed exists", `#${channel.name}: ${post.marker}`);
      return;
    }

    action("update seed message", `#${channel.name}: ${post.marker}`);
    await discord("PATCH", `/channels/${channel.id}/messages/${existingMessage.id}`, payload);
    return;
  }

  action("send seed message", `#${channel.name}: ${post.marker}`);
  await discord("POST", `/channels/${channel.id}/messages`, payload);
}

function sameComponents(left, right) {
  return JSON.stringify(left) === JSON.stringify(right);
}

async function ensureMemberRole(userId, role, label) {
  const member = await discord("GET", `/guilds/${guildId}/members/${userId}`);
  if (member.roles?.includes(role.id)) {
    action("member role exists", `${label}: ${role.name}`);
    return;
  }

  action("assign member role", `${label}: ${role.name}`);
  if (dryRun) return;

  await discord("PUT", `/guilds/${guildId}/members/${userId}/roles/${role.id}`);
}

async function discord(method, path, body, attempt = 0) {
  const response = await fetch(`${API_BASE}${path}`, {
    method,
    headers: {
      Authorization: `Bot ${token}`,
      "Content-Type": "application/json",
      "X-Audit-Log-Reason": auditReason,
    },
    body: body == null ? undefined : JSON.stringify(body),
  });

  if (response.status === 429) {
    const rateLimit = await response.json().catch(() => ({}));
    const retryAfterMs = Math.ceil(Number(rateLimit.retry_after ?? 1) * 1000) + 250;
    if (attempt > 5) throw new Error(`Discord rate limit did not clear for ${method} ${path}`);
    await sleep(retryAfterMs);
    return discord(method, path, body, attempt + 1);
  }

  if (!response.ok) {
    const text = await response.text();
    throw new Error(`${method} ${path} failed with ${response.status}: ${text}`);
  }

  if (response.status === 204) return null;
  return response.json();
}

function printInviteUrl() {
  const clientId = process.env.DISCORD_CLIENT_ID;
  if (!clientId) throw new Error("Set DISCORD_CLIENT_ID before using --invite.");

  const params = new URLSearchParams({
    client_id: clientId,
    permissions: INVITE_PERMISSIONS,
    scope: "bot applications.commands",
  });

  if (process.env.DISCORD_GUILD_ID) {
    params.set("guild_id", process.env.DISCORD_GUILD_ID);
    params.set("disable_guild_select", "true");
  }

  console.log(`Invite permissions bitfield: ${INVITE_PERMISSIONS}`);
  console.log(`https://discord.com/oauth2/authorize?${params.toString()}`);
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

function mapByLowerName(items) {
  return new Map(items.map((item) => [normalizeName(item.name), item]));
}

function findByDesiredName(itemMap, desired) {
  for (const name of namesForDesired(desired)) {
    const existing = itemMap.get(name);
    if (existing) return existing;
  }
  return null;
}

function setDesiredItem(itemMap, item, desired) {
  if (desired.key) itemMap.set(desired.key, item);
  for (const name of namesForDesired(desired)) {
    itemMap.set(name, item);
  }
  itemMap.set(normalizeName(item.name), item);
}

function indexDesiredItems(items, desiredItems) {
  const itemMap = mapByLowerName(items);
  const indexed = new Map(itemMap);
  for (const desired of desiredItems) {
    const existing = findByDesiredName(itemMap, desired);
    if (existing) indexed.set(desired.key, existing);
  }
  return indexed;
}

function namesForDesired(desired) {
  return [desired.name, ...(desired.aliases ?? [])].map(normalizeName);
}

function normalizeName(name) {
  return name.toLowerCase();
}

function action(kind, detail) {
  console.log(`${dryRun ? "[dry-run]" : "[apply]"} ${kind}: ${detail}`);
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function usage() {
  console.log(`
Usage:
  node tools/discord/setup-server.mjs --invite
  node tools/discord/setup-server.mjs
  node tools/discord/setup-server.mjs --apply
  node tools/discord/setup-server.mjs --apply --seed

Environment:
  DISCORD_CLIENT_ID   Application client ID, only needed for --invite.
  DISCORD_GUILD_ID    Bitcoin 09 server ID.
  DISCORD_BOT_TOKEN   Bot token. Never commit this.

Default mode is dry-run. Use --apply to change Discord.
`);
}
