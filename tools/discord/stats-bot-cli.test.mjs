import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { classifyInteraction, formatStatsMessage, getStats } from "./stats-bot.mjs";

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
  for (const mode of ["--post", "--register-commands"]) {
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
};

function jsonResponse(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "content-type": "application/json" },
  });
}

test("chain stats remain useful when every third-party pool request fails", async () => {
  const stats = await getStats(async (url) => {
    if (url.includes("explorer.btc09.org")) return jsonResponse(explorerStatus);
    throw new Error("third-party pool offline");
  });
  const message = formatStatsMessage(stats);
  assert.match(message, /Network height \/ peers: \*\*7,386 \/ 6\*\*/);
  assert.match(message, /Estimated network hashrate: \*\*16\.30 KH\/s\*\*/);
  assert.match(message, /Top payout address, last 100 blocks: \*\*100\.0%/);
  assert.doesNotMatch(message, /Community pool/);
});

test("available pool stats are appended and clearly marked third-party", async () => {
  const stats = await getStats(async (url) => {
    if (url.includes("explorer.btc09.org")) return jsonResponse(explorerStatus);
    if (url.includes("/miners")) return jsonResponse([{ miner: "09c-address", hashrate: 1200 }]);
    if (url.includes("/blocks")) return jsonResponse([{ miner: "09c-address" }]);
    if (url.includes("/payments")) return jsonResponse([{ address: "09c-address" }]);
    return jsonResponse({
      pool: {
        poolStats: { connectedMiners: 1, poolHashrate: 1200 },
        totalBlocks: 12,
        totalPaid: 50,
      },
    });
  });
  const message = formatStatsMessage(stats);
  assert.match(message, /Community pool \(third-party\)/);
  assert.match(message, /Active pool payout addresses: \*\*1\*\*/);
  assert.match(message, /09c-address/);
});

test("registered stats commands and role buttons are routed", () => {
  assert.equal(classifyInteraction({ type: 2, data: { name: "stats" } }), "stats");
  assert.equal(classifyInteraction({ type: 3, data: { custom_id: "role:toggle:miner" } }), "role");
  assert.equal(classifyInteraction({ type: 2, data: { name: "unknown" } }), null);
});
