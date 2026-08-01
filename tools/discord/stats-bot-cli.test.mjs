import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { classifyInteraction, formatStatsMessage, getStats } from "./stats-bot.mjs";
import * as statsBot from "./stats-bot.mjs";

const scriptPath = fileURLToPath(new URL("./stats-bot.mjs", import.meta.url));
const missingDiscordEnv = {
  ...process.env,
  DISCORD_CLIENT_ID: "",
  DISCORD_GUILD_ID: "",
  DISCORD_BOT_TOKEN: "",
};

function run(mode) {
  return spawnSync(process.execPath, [scriptPath, mode], {
    encoding: "utf8",
    env: missingDiscordEnv,
  });
}

test("one-shot Discord commands fail when required credentials are missing", () => {
  for (const mode of ["--post", "--post-exchanges", "--register-commands"]) {
    const result = run(mode);
    assert.equal(result.status, 1, `${mode} should exit nonzero`);
    assert.match(result.stderr, /Missing required environment variables/);
  }
});

test("watch exits cleanly on terminal startup configuration errors", () => {
  const result = run("--watch");
  assert.equal(result.status, 0);
  assert.match(result.stderr, /Missing required environment variables/);
});

test("CLI help lists every Node-owned guild command", () => {
  const result = run("--help");
  assert.equal(result.status, 0);
  assert.match(
    result.stdout,
    /\/stats, \/rank, \/leaderboard, \/wallet, \/mine, and \/support commands/,
  );
});

const explorerStatus = {
  height: 7386,
  peers: 6,
  difficulty: 64,
  estimated_network_hashrate_hps: 16302.9,
  target_block_seconds: 600,
  epoch_average_block_seconds: 257.3,
  blocks_to_retarget: 678,
  next_retarget_height: 8064,
  estimated_next_difficulty: 149.26,
  difficulty_algorithm: "legacy-2016",
  asert_activation_height: 12096,
  blocks_to_asert: 4710,
  asert_half_life_seconds: 7200,
  highest_advertised_peer_height: 7390,
  advertised_peer_height_lag: 4,
  payout_address_windows: [
    {
      requested_blocks: 100,
      observed_blocks: 100,
      distinct_payout_addresses: 1,
      top_payout_address: "4k26VjMfx4sNQ1pdr7N4DJCY126xezv4Rb",
      top_payout_blocks: 100,
      top_share_percent: 100,
    },
  ],
  block_source_windows: [
    {
      requested_blocks: 100,
      observed_blocks: 100,
      solo_blocks: 80,
      distributed_blocks: 20,
      distinct_solo_payout_addresses: 4,
      top_solo_payout_address: "4k26VjMfx4sNQ1pdr7N4DJCY126xezv4Rb",
      top_solo_payout_blocks: 72,
      top_solo_share_percent: 72,
    },
  ],
};

function jsonResponse(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json" },
  });
}

test("chain stats use the official explorer without calling miner download or pool services", async () => {
  const calls = [];
  const stats = await getStats(async (url) => {
    calls.push(url);
    if (url.includes("explorer.btc09.org")) return jsonResponse(explorerStatus);
    throw new Error(`unexpected service ${url}`);
  });
  const message = formatStatsMessage(stats);
  assert.deepEqual(calls, ["https://explorer.btc09.org/api/status"]);
  assert.match(message, /Network height \/ peers: \*\*7,386 \/ 6\*\*/);
  assert.match(message, /Estimated network hashrate: \*\*16\.30 KH\/s\*\*/);
  assert.match(message, /Top solo payout address, last 100 blocks: \*\*72\.0%/);
  assert.match(message, /Distributed\/multi-output blocks, last 100: \*\*20\*\*/);
  assert.match(message, /ASERT: \*\*4,710 blocks\*\*, height \*\*12,096\*\*/);
  assert.match(message, /Highest advertised peer tip: \*\*7,390\*\* \(local node 4 blocks behind\)/);
  assert.doesNotMatch(message, /Community pool/);
  assert.doesNotMatch(message, /ntmminer/i);
});

test("chain stats report ASERT without a fake legacy retarget", () => {
  const active = {
    ...explorerStatus,
    difficulty_algorithm: "asert",
    blocks_to_asert: 0,
    blocks_to_retarget: 0,
    next_retarget_height: 0,
  };
  const message = formatStatsMessage({ explorer: active, checkedAt: new Date(0) });
  assert.match(message, /Difficulty adjustment: \*\*ASERT active\*\*, 2h half-life/);
  assert.doesNotMatch(message, /Retarget:/);
  assert.doesNotMatch(message, /retarget in 0/i);
});

test("exchange status is concise, factual, and links to the public tracker", () => {
  const message = statsBot.formatExchangeStatusMessage({
    updated_at: "2026-08-01T08:13:00Z",
    summary: {
      awaiting_reply: 16,
      terms_requested: 1,
      requirements_needed: 10,
      engineering_needed: 2,
      paid_routes_published: 2,
    },
    funding: {
      cash_received_usd: 0,
      cash_target_usd: 3899,
      coin_liquidity_received_usd: 0,
      coin_liquidity_target_usd: 400,
    },
  });

  assert.match(message, /Submitted \/ awaiting: \*\*16\*\*/);
  assert.match(message, /Requirements \/ engineering: \*\*12\*\*/);
  assert.match(message, /US\$0 \/ US\$3,899 cash/);
  assert.match(message, /Applications are pending reviews, not listings\./);
  assert.match(message, /https:\/\/btc09\.org\/exchanges\.html/);
  assert.ok(message.length < 700);
  assert.doesNotMatch(message, /approved|partnership|guaranteed/i);
  assert.doesNotMatch(message, /—/);
});

test("exchange status uses the confirmed funding service without failing closed", async () => {
  const config = {
    updated_at: "2026-08-01T08:13:00Z",
    summary: { awaiting_reply: 15 },
    funding: { cash_received_usd: 0, cash_target_usd: 3899 },
  };
  const calls = [];
  const status = await statsBot.getExchangeStatus(async (url) => {
    calls.push(url);
    if (url.endsWith("/exchanges.json")) return jsonResponse(config);
    if (url.endsWith("/api/support/v1/status")) {
      return jsonResponse({ cash_received_usd: 125, confirmed_payments: 3 });
    }
    throw new Error(`unexpected service ${url}`);
  });
  assert.deepEqual(calls, [
    "https://btc09.org/exchanges.json",
    "https://btc09.org/api/support/v1/status",
  ]);
  assert.equal(status.funding.cash_received_usd, 125);
  assert.equal(status.funding.cash_target_usd, 3899);

  const fallback = await statsBot.getExchangeStatus(async (url) => {
    if (url.endsWith("/exchanges.json")) return jsonResponse(config);
    throw new Error("funding service offline");
  });
  assert.equal(fallback.funding.cash_received_usd, 0);
});

test("stats implementation contains no retired pool dependency", async () => {
  const source = await import("node:fs/promises").then(({ readFile }) =>
    readFile(scriptPath, "utf8"),
  );
  assert.doesNotMatch(source, /ntmminer/i);
  assert.doesNotMatch(source, /POOL_BASE/);
});

test("registered stats commands and role buttons are routed", () => {
  assert.equal(classifyInteraction({ type: 2, data: { name: "stats" } }), "stats");
  assert.equal(classifyInteraction({ type: 2, data: { name: "rank" } }), "rank");
  assert.equal(classifyInteraction({ type: 2, data: { name: "leaderboard" } }), "leaderboard");
  assert.equal(classifyInteraction({ type: 2, data: { name: "wallet" } }), "wallet");
  assert.equal(classifyInteraction({ type: 2, data: { name: "mine" } }), "mine");
  assert.equal(classifyInteraction({ type: 2, data: { name: "support" } }), "support");
  assert.equal(classifyInteraction({ type: 3, data: { custom_id: "role:toggle:miner" } }), "role");
  assert.equal(classifyInteraction({ type: 2, data: { name: "unknown" } }), null);
  assert.deepEqual(statsBot.getCommandDefinitions().map(({ name }) => name), [
    "stats",
    "rank",
    "leaderboard",
    "wallet",
    "mine",
    "support",
  ]);
});

test("supporter copy is concise and does not promise an investment return", () => {
  const message = statsBot.formatSupportPerks();
  for (const token of [
    "US$5: 💛 Supporter",
    "US$25: 🤝 Backer + supporter lab",
    "US$100: 🛠 Builder",
    "US$250: ⭐ Core Supporter",
    "cumulative finished BTC09 support payments",
    "`/support claim`",
  ]) {
    assert.match(message, new RegExp(token.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  }
  assert.ok(message.length < 700);
  assert.match(message, /not 09C, equity, governance, or a return/);
  assert.doesNotMatch(message, /guarantee|profit|yield/i);
});

test("support claims use the loopback service secret and validate its tier", async () => {
  const calls = [];
  const claimCode = "a".repeat(32);
  const claimed = await statsBot.claimSupportPayment({
    claimCode,
    userId: "123456789012345678",
    claimSecret: "test-secret",
    fetchImpl: async (url, options) => {
      calls.push({ url, options });
      return jsonResponse({
        claimed: true,
        total_confirmed_usd: 30,
        tier: {
          key: "backer",
          role_name: "🤝 Backer",
          supporter_lab: true,
        },
      });
    },
  });
  assert.equal(claimed.tier.key, "backer");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "http://127.0.0.1:8032/internal/support/v1/claims");
  assert.equal(calls[0].options.headers["X-BTC09-Claim-Secret"], "test-secret");
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    claim_code: claimCode,
    discord_user_id: "123456789012345678",
  });

  await assert.rejects(statsBot.claimSupportPayment({
    claimCode: "short",
    userId: "123456789012345678",
    claimSecret: "test-secret",
    fetchImpl: async () => { throw new Error("must not fetch"); },
  }), /claim code is invalid/);
});

test("wallet and mining help is short, current, and points to official pages", () => {
  const wallet = statsBot.formatWalletHelp();
  const mining = statsBot.formatMiningHelp();
  assert.match(wallet, /Open the BTC09 Wallet/);
  assert.match(wallet, /Send, receive, Activity/);
  assert.match(wallet, /https:\/\/btc09\.org\/#download/);
  assert.match(wallet, /recovery words/i);
  assert.match(mining, /Mine tab/);
  assert.match(mining, /PPLNS/);
  assert.match(mining, /0% pool fee/);
  assert.match(mining, /100 confirmations/);
  assert.match(mining, /https:\/\/btc09\.org\/#mining-guide/);
  for (const message of [wallet, mining]) {
    assert.ok(message.length < 700, `help is too long: ${message.length}`);
    assert.doesNotMatch(message, /—/);
  }
});

test("XP rank roles are created once and displayed separately", async () => {
  assert.equal(typeof statsBot.syncXpRankRoles, "function");
  const roles = [{ id: "everyone", name: "@everyone" }];
  const calls = [];
  let nextId = 1;
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/roles") {
      return roles.map((role) => ({ ...role }));
    }
    if (method === "POST" && path === "/guilds/guild-1/roles") {
      const role = { id: `rank-${nextId++}`, ...body };
      roles.push(role);
      return { ...role };
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  const created = await statsBot.syncXpRankRoles("guild-1", { discordImpl });
  assert.deepEqual(created.map(({ name }) => name), ["🌱 Active", "⭐ Regular", "🏆 Veteran"]);
  assert.ok(created.every((role) => role.hoist === true));
  assert.ok(created.every((role) => role.permissions === "0"));

  calls.length = 0;
  await statsBot.syncXpRankRoles("guild-1", { discordImpl });
  assert.equal(calls.filter((call) => call.method === "POST").length, 0);
});

test("XP promotion keeps only the highest earned rank role", async () => {
  assert.equal(typeof statsBot.applyXpRankRoles, "function");
  const calls = [];
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    return null;
  };
  const roles = [
    { id: "active", name: "🌱 Active" },
    { id: "regular", name: "⭐ Regular" },
    { id: "veteran", name: "🏆 Veteran" },
  ];

  await statsBot.applyXpRankRoles({
    guildId: "guild-1",
    userId: "user-1",
    memberRoleIds: ["active"],
    level: 8,
    roles,
    discordImpl,
  });

  assert.deepEqual(calls, [
    {
      method: "DELETE",
      path: "/guilds/guild-1/members/user-1/roles/active",
      body: null,
    },
    {
      method: "PUT",
      path: "/guilds/guild-1/members/user-1/roles/regular",
      body: null,
    },
  ]);
});

test("supporter setup creates four display roles and one private lab without duplicates", async () => {
  const roles = [{ id: "guild-1", name: "@everyone", color: 0, permissions: "0" }];
  const channels = [{ id: "community", name: "💬 COMMUNITY", type: 4, position: 1 }];
  const calls = [];
  let nextRole = 1;
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/roles") {
      return roles.map((role) => ({ ...role }));
    }
    if (method === "POST" && path === "/guilds/guild-1/roles") {
      const role = { id: `support-${nextRole++}`, ...body };
      roles.push(role);
      return { ...role };
    }
    if (method === "GET" && path === "/guilds/guild-1/channels") {
      return channels.map((channel) => ({ ...channel }));
    }
    if (method === "POST" && path === "/guilds/guild-1/channels") {
      const channel = { id: "supporter-lab", ...body };
      channels.push(channel);
      return { ...channel };
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  const first = await statsBot.syncSupporterInfrastructure("guild-1", {
    clientId: "bot-1",
    discordImpl,
  });
  assert.deepEqual(first.roles.map((role) => role.name), [
    "💛 Supporter",
    "🤝 Backer",
    "🛠 Builder",
    "⭐ Core Supporter",
  ]);
  assert.ok(first.roles.every((role) => role.hoist === true));
  assert.equal(first.channel.name, "💛-supporter-lab");
  assert.equal(first.channel.parent_id, "community");
  assert.deepEqual(first.channel.permission_overwrites[0], {
    id: "guild-1", type: 0, allow: "0", deny: "1024",
  });
  assert.equal(first.channel.permission_overwrites.some((overwrite) =>
    overwrite.id === first.roles[0].id
  ), false);
  for (const role of first.roles.slice(1)) {
    assert.equal(first.channel.permission_overwrites.some((overwrite) =>
      overwrite.id === role.id && overwrite.allow === "84992"
    ), true);
  }

  calls.length = 0;
  await statsBot.syncSupporterInfrastructure("guild-1", {
    clientId: "bot-1",
    discordImpl,
  });
  assert.deepEqual(calls.map(({ method, path }) => ({ method, path })), [
    { method: "GET", path: "/guilds/guild-1/roles" },
    { method: "GET", path: "/guilds/guild-1/channels" },
  ]);
});

test("supporter lab intro is useful and idempotent", async () => {
  const content = statsBot.formatSupporterLabIntro();
  assert.match(content, /early wallet and miner builds/);
  assert.match(content, /No price calls, paid hype, or guaranteed roadmap slots/);
  assert.ok(content.length < 500);

  const messages = [];
  const calls = [];
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/users/@me") return { id: "bot-1" };
    if (method === "GET" && path === "/channels/lab/messages?limit=50") {
      return messages.map((message) => ({ ...message }));
    }
    if (method === "POST" && path === "/channels/lab/messages") {
      const message = { id: "intro-1", author: { id: "bot-1" }, ...body };
      messages.push(message);
      return { ...message };
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };
  assert.equal(await statsBot.postOrUpdateSupporterLabIntro({ id: "lab" }, { discordImpl }), "intro-1");
  calls.length = 0;
  assert.equal(await statsBot.postOrUpdateSupporterLabIntro({ id: "lab" }, { discordImpl }), "intro-1");
  assert.equal(calls.some((call) => call.method === "POST" || call.method === "PATCH"), false);
});

test("supporter promotion keeps only the highest confirmed tier role", async () => {
  const calls = [];
  const roles = [
    { id: "supporter", name: "💛 Supporter" },
    { id: "backer", name: "🤝 Backer" },
    { id: "builder", name: "🛠 Builder" },
    { id: "core", name: "⭐ Core Supporter" },
  ];
  await statsBot.applySupporterRole({
    guildId: "guild-1",
    userId: "user-1",
    memberRoleIds: ["supporter"],
    tierKey: "builder",
    roles,
    discordImpl: async (method, path, body) => {
      calls.push({ method, path, body });
      return null;
    },
  });
  assert.deepEqual(calls, [
    {
      method: "DELETE",
      path: "/guilds/guild-1/members/user-1/roles/supporter",
      body: null,
    },
    {
      method: "PUT",
      path: "/guilds/guild-1/members/user-1/roles/builder",
      body: null,
    },
  ]);
});

test("live stats channel names are compact and come from official explorer data", () => {
  assert.equal(typeof statsBot.formatLiveStatChannelNames, "function");
  assert.deepEqual(statsBot.formatLiveStatChannelNames({ explorer: explorerStatus }), [
    "🧱 Height: 7,386",
    "⚡ Hashrate: 16.30 KH/s",
    "⛏ Difficulty: 64.00",
    "🌐 Peers: 6",
  ]);
});

test("live stats sync creates a locked category at the top and is idempotent", async () => {
  assert.equal(typeof statsBot.syncLiveStatChannels, "function");
  const calls = [];
  const channels = [];
  let nextId = 1;
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/channels") {
      return channels.map((channel) => ({ ...channel }));
    }
    if (method === "POST" && path === "/guilds/guild-1/channels") {
      const channel = { id: `channel-${nextId++}`, ...body };
      channels.push(channel);
      return { ...channel };
    }
    if (method === "PATCH" && path.startsWith("/channels/")) {
      const channel = channels.find((item) => item.id === path.split("/").at(-1));
      Object.assign(channel, body);
      return { ...channel };
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  await statsBot.syncLiveStatChannels(
    { explorer: explorerStatus },
    { guildId: "guild-1", clientId: "bot-1", discordImpl },
  );

  const creates = calls.filter((call) => call.method === "POST");
  assert.equal(creates.length, 5);
  assert.deepEqual(creates[0].body, {
    name: "📊 LIVE STATS",
    type: 4,
    position: 0,
    permission_overwrites: [
      { id: "guild-1", type: 0, allow: "0", deny: "1048576" },
      { id: "bot-1", type: 1, allow: "1048576", deny: "0" },
    ],
  });
  assert.deepEqual(
    creates.slice(1).map((call) => call.body.name),
    statsBot.formatLiveStatChannelNames({ explorer: explorerStatus }),
  );
  assert.ok(creates.slice(1).every((call) => call.body.type === 2));
  assert.ok(creates.slice(1).every((call) => call.body.parent_id === "channel-1"));
  assert.ok(creates.slice(1).every((call) =>
    call.body.permission_overwrites.some((overwrite) =>
      overwrite.id === "bot-1" && overwrite.allow === "1048576"
    )
  ));

  calls.length = 0;
  await statsBot.syncLiveStatChannels(
    { explorer: explorerStatus },
    { guildId: "guild-1", clientId: "bot-1", discordImpl },
  );
  assert.deepEqual(calls, [
    { method: "GET", path: "/guilds/guild-1/channels", body: undefined },
  ]);
});

test("live stats sync explicitly keeps LIVE STATS above INFO", async () => {
  const names = statsBot.formatLiveStatChannelNames({ explorer: explorerStatus });
  const channels = [
    { id: "info", name: "📌 INFO", type: 4, position: 0 },
    {
      id: "live",
      name: "📊 LIVE STATS",
      type: 4,
      position: 0,
      permission_overwrites: [
        { id: "guild-1", type: 0, allow: "0", deny: "1048576" },
      ],
    },
    ...names.map((name, position) => ({
      id: `stat-${position}`,
      name,
      type: 2,
      parent_id: "live",
      position,
    })),
  ];
  const calls = [];
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/channels") {
      return channels.map((channel) => ({ ...channel }));
    }
    if (method === "PATCH" && path === "/guilds/guild-1/channels") {
      return body;
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  await statsBot.syncLiveStatChannels(
    { explorer: explorerStatus },
    { guildId: "guild-1", clientId: null, discordImpl },
  );

  assert.deepEqual(calls.at(-1), {
    method: "PATCH",
    path: "/guilds/guild-1/channels",
    body: [
      { id: "live", position: 0 },
      { id: "info", position: 1 },
    ],
  });
});

test("live stats category lets the bot update locked voice rows", async () => {
  const names = statsBot.formatLiveStatChannelNames({ explorer: explorerStatus });
  const category = {
    id: "live",
    name: "📊 LIVE STATS",
    type: 4,
    position: 0,
    permission_overwrites: [
      { id: "guild-1", type: 0, allow: "0", deny: "1048576" },
    ],
  };
  const channels = [
    category,
    ...names.map((name, position) => ({
      id: `stat-${position}`,
      name,
      type: 2,
      parent_id: "live",
      position,
    })),
  ];
  const calls = [];
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/channels") {
      return channels.map((channel) => ({ ...channel }));
    }
    if (method === "PATCH" && path === "/channels/live") {
      return { ...category, ...body };
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  await statsBot.syncLiveStatChannels(
    { explorer: explorerStatus },
    { guildId: "guild-1", clientId: "bot-1", discordImpl },
  );

  assert.deepEqual(calls.at(1), {
    method: "PATCH",
    path: "/channels/live",
    body: {
      permission_overwrites: [
        { id: "guild-1", type: 0, allow: "0", deny: "1048576" },
        { id: "bot-1", type: 1, allow: "1048576", deny: "0" },
      ],
    },
  });
});

test("live stats sync keeps child permission overwrites aligned", async () => {
  const names = statsBot.formatLiveStatChannelNames({ explorer: explorerStatus });
  const categoryPermissions = [
    { id: "guild-1", type: 0, allow: "0", deny: "1048576" },
    { id: "bot-1", type: 1, allow: "1048576", deny: "0" },
  ];
  const channels = [
    {
      id: "live",
      name: "📊 LIVE STATS",
      type: 4,
      position: 0,
      permission_overwrites: categoryPermissions,
    },
    ...names.map((name, position) => ({
      id: `stat-${position}`,
      name,
      type: 2,
      parent_id: "live",
      position,
      permission_overwrites: [categoryPermissions[0]],
    })),
  ];
  const calls = [];
  const discordImpl = async (method, path, body) => {
    calls.push({ method, path, body });
    if (method === "GET" && path === "/guilds/guild-1/channels") {
      return channels.map((channel) => ({ ...channel }));
    }
    if (method === "PATCH" && path.startsWith("/channels/stat-")) {
      return body;
    }
    throw new Error(`unexpected Discord call ${method} ${path}`);
  };

  await statsBot.syncLiveStatChannels(
    { explorer: explorerStatus },
    { guildId: "guild-1", clientId: "bot-1", discordImpl },
  );

  assert.deepEqual(calls.slice(1), [
    ...names.map((name, position) => ({
      method: "PATCH",
      path: `/channels/stat-${position}`,
      body: {
        name,
        parent_id: "live",
        position,
        permission_overwrites: categoryPermissions,
      },
    })),
  ]);
});

test("combined live stats refresh still updates the message when channel renames fail", async () => {
  const calls = [];
  const errors = [];

  await statsBot.refreshAllLiveStats({
    channelRefreshImpl: async () => {
      calls.push("channels");
      throw new Error("Missing Access");
    },
    messageRefreshImpl: async () => {
      calls.push("message");
    },
    exchangeMessageRefreshImpl: async () => {
      calls.push("exchange");
    },
    logger: { error: (...args) => errors.push(args.join(" ")) },
  });

  assert.deepEqual(calls, ["channels", "message", "exchange"]);
  assert.deepEqual(errors, ["Live stats channel update failed: Missing Access"]);
});

test("watch updater refreshes immediately and every ten minutes", async () => {
  assert.equal(typeof statsBot.startLiveStatsUpdater, "function");
  const intervals = [];
  let refreshes = 0;
  const refreshImpl = async () => {
    refreshes += 1;
  };
  const setIntervalImpl = (callback, milliseconds) => {
    intervals.push({ callback, milliseconds });
    return 99;
  };

  const timer = statsBot.startLiveStatsUpdater({
    refreshImpl,
    setIntervalImpl,
    logger: { error() {} },
  });
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(timer, 99);
  assert.equal(refreshes, 1);
  assert.equal(intervals.length, 1);
  assert.equal(intervals[0].milliseconds, 600_000);

  await intervals[0].callback();
  assert.equal(refreshes, 2);
});
