import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";

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
