import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { chromium } from "playwright";

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/(.:)/, "$1")), "..", "..");
const defaultCore = path.join(root, "walletapp", "src-tauri", "target", "release", "btc09-core.exe");
const corePath = path.resolve(process.argv[2] || defaultCore);
const walletOnly = process.env.BTC09_SMOKE_WALLET_ONLY === "1";
const temporaryRoot = await mkdtemp(path.join(os.tmpdir(), "btc09-native-smoke-"));
const walletPath = path.join(temporaryRoot, "wallet-mainnet.json");
let backend;
let browser;

function firstLine(stream, timeoutMs) {
  return new Promise((resolve, reject) => {
    const lines = readline.createInterface({ input: stream, crlfDelay: Infinity });
    const timer = setTimeout(() => {
      lines.close();
      reject(new Error("BTC09 Core did not publish its startup information."));
    }, timeoutMs);
    lines.once("line", (line) => {
      clearTimeout(timer);
      lines.close();
      resolve(line);
    });
  });
}

async function stopBackend() {
  if (!backend || backend.exitCode !== null) return;
  backend.stdin.end();
  const exited = new Promise((resolve) => backend.once("exit", resolve));
  const timedOut = new Promise((resolve) => setTimeout(() => resolve("timeout"), 6000));
  if ((await Promise.race([exited, timedOut])) === "timeout") {
    backend.kill();
    await exited;
  }
}

async function assertPrimaryActionFits(page, width, height) {
  await page.setViewportSize({ width, height });
  await page.evaluate(() => scrollTo(0, 0));
  const bounds = await page.locator("#create-wallet").boundingBox();
  assert.ok(bounds, "Create wallet is not visible.");
  assert.ok(bounds.y + bounds.height <= height, `Create wallet falls below ${width}x${height}.`);
}

try {
  const coreArguments = ["app", "-desktop-host"];
  if (walletOnly) coreArguments.push("-wallet-only");
  coreArguments.push("-datadir", temporaryRoot);
  backend = spawn(corePath, coreArguments, {
    env: { ...process.env, BTC09_WALLET_PATH: walletPath },
    stdio: ["pipe", "pipe", "pipe"],
    windowsHide: true,
  });
  const launch = JSON.parse(await firstLine(backend.stdout, 15000));
  assert.equal(launch.schema_version, 1);
  assert.equal(launch.version, "v0.1.33");
  assert.match(launch.launch_url, /^http:\/\/127\.0\.0\.1:\d+\?token=[a-f0-9]{64}$/);

  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext();
  const page = await context.newPage();
  const runtimeErrors = [];
  page.on("pageerror", (error) => runtimeErrors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") runtimeErrors.push(message.text());
  });
  page.on("requestfailed", (request) => runtimeErrors.push(`${request.method()} ${new URL(request.url()).pathname} failed`));

  await page.goto(launch.launch_url, { waitUntil: "networkidle" });
  await page.locator("#first-run").waitFor({ state: "visible" });
  await assertPrimaryActionFits(page, 1180, 790);
  await assertPrimaryActionFits(page, 760, 700);

  const password = "temporary-test-wallet";
  await page.locator("#create-password").fill(password);
  await page.locator("#create-password-confirm").fill(password);
  await page.locator("#wallet-safety-ack").check();
  await page.locator("#create-wallet").click();
  await page.locator("#recovery-backup").waitFor({ state: "visible" });
  const words = await page.locator("#recovery-word-grid li span").allTextContents();
  assert.equal(words.length, 24, "Recovery backup did not contain 24 words.");
  await page.locator("#start-recovery-confirm").click();
  await page.locator("#confirm-word-4").fill(words[3]);
  await page.locator("#confirm-word-12").fill(words[11]);
  await page.locator("#confirm-word-21").fill(words[20]);
  await page.locator("#confirm-recovery-backup").click();
  await page.locator("#wallet-view").waitFor({ state: "visible" });
  assert.equal(await page.locator("#balance-major").textContent(), "0");

  await page.locator('[data-panel="activity-panel"]').click();
  await page.locator("#activity-list").waitFor({ state: "visible" });
  await page.waitForFunction(() => !document.querySelector("#activity-list")?.textContent?.includes("load wallet history"));
  assert.ok(!/networkerror|failed to fetch/i.test(await page.locator("body").innerText()));

  if (walletOnly) {
    await page.waitForFunction(() => document.querySelector('[data-panel="miner-panel"]')?.hidden === true);
    assert.equal(await page.locator('[data-panel="miner-panel"]').isHidden(), true);
  } else {
    await page.locator('[data-panel="miner-panel"]').click();
    await page.locator("#miner-workers").fill("1");
    await page.locator("#start-miner").click();
    await page.waitForFunction(() => document.querySelector("#miner-state")?.textContent?.trim() !== "Stopped");
    await page.waitForTimeout(2000);
    await page.locator("#stop-miner").click();
    await page.waitForFunction(() => document.querySelector("#miner-state")?.textContent?.trim() === "Stopped");
  }

  assert.deepEqual(runtimeErrors, [], `Browser errors: ${runtimeErrors.join(" | ")}`);
  process.stdout.write(
    walletOnly
      ? "BTC09 wallet-only smoke test passed: setup, activity, and no on-device miner.\n"
      : "BTC09 native wallet smoke test passed: setup, activity, and one-thread miner start/stop.\n",
  );
} finally {
  if (browser) await browser.close();
  await stopBackend();
  const resolvedTemp = path.resolve(os.tmpdir()) + path.sep;
  const resolvedWork = path.resolve(temporaryRoot);
  if (!resolvedWork.startsWith(resolvedTemp)) {
    throw new Error("Refusing to remove a test folder outside the system temp directory.");
  }
  await rm(resolvedWork, { recursive: true, force: true });
}
