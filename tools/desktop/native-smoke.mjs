import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { chromium } from "playwright";

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/(.:)/, "$1")), "..", "..");
const defaultCore = path.join(root, "walletapp", "src-tauri", "target", "release", "btc09-core.exe");
const corePath = path.resolve(process.argv[2] || defaultCore);
const walletOnly = process.env.BTC09_SMOKE_WALLET_ONLY === "1";
const screenshotDirectory = process.env.BTC09_SMOKE_SCREENSHOT_DIR
  ? path.resolve(process.env.BTC09_SMOKE_SCREENSHOT_DIR)
  : "";
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

async function assertDesktopShellVisible(page) {
  assert.equal(await page.evaluate(() => window.scrollY), 0, "Desktop navigation scrolled the whole app shell.");
  const topbar = await page.locator(".topbar").boundingBox();
  assert.ok(topbar, "Desktop header is not visible.");
  assert.ok(topbar.y >= 0 && topbar.y + topbar.height <= 220, "Desktop header moved outside the viewport.");
}

async function assertInstrumentDesign(page) {
  const design = await page.evaluate(() => {
    const frame = getComputedStyle(document.querySelector(".wallet-frame"));
    const mark = getComputedStyle(document.querySelector(".wordmark-mark"));
    const main = getComputedStyle(document.querySelector("main"));
    return {
      columns: frame.gridTemplateColumns,
      frameRadius: Number.parseFloat(frame.borderRadius),
      markRadius: Number.parseFloat(mark.borderRadius),
      mainBackground: main.backgroundColor,
      bodyFont: getComputedStyle(document.body).fontFamily,
    };
  });
  assert.match(
    design.columns,
    /^236px /,
    `Packaged desktop wallet should use the instrument rail: ${JSON.stringify(design)}`,
  );
  assert.ok(design.frameRadius <= 3, "Desktop shell should not return to a floating rounded card.");
  assert.ok(design.markRadius <= 3, "The 09 mark should stay square.");
  assert.equal(design.mainBackground, "rgb(238, 242, 238)");
  assert.doesNotMatch(design.bodyFont, /Georgia|Palatino|Book Antiqua/i);
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
  assert.equal(launch.version, "v0.1.34");
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
  await assertInstrumentDesign(page);
  await page.locator("#first-run").evaluate(async () => {
    await Promise.all(document.getAnimations({ subtree: true }).map((animation) => animation.finished.catch(() => undefined)));
  });
  if (screenshotDirectory) {
    await mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDirectory, walletOnly ? "store-onboarding.png" : "desktop-onboarding.png") });
  }
  await assertPrimaryActionFits(page, 760, 700);
  await page.setViewportSize({ width: 1180, height: 790 });
  await page.evaluate(() => scrollTo(0, 0));

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
  await page.locator("#wallet-view").evaluate(async () => {
    await Promise.all(document.getAnimations({ subtree: true }).map((animation) => animation.finished.catch(() => undefined)));
  });
  assert.equal(await page.locator("#balance-major").textContent(), "0");
  const walletDesign = await page.evaluate(() => ({
    summaryBackground: getComputedStyle(document.querySelector(".account-summary")).backgroundColor,
    tabRadius: Number.parseFloat(getComputedStyle(document.querySelector(".ledger-tab")).borderRadius),
    ledgerRadius: Number.parseFloat(getComputedStyle(document.querySelector(".ledger")).borderRadius),
  }));
  assert.equal(walletDesign.summaryBackground, "rgb(18, 60, 50)");
  assert.ok(walletDesign.tabRadius <= 3, "Desktop actions should use the square instrument geometry.");
  assert.ok(walletDesign.ledgerRadius <= 3, "Desktop content should stay flat and inspectable.");

  if (screenshotDirectory) {
    await page.screenshot({ path: path.join(screenshotDirectory, walletOnly ? "store-receive.png" : "desktop-receive.png") });
  }
  await page.locator('[data-panel="send-panel"]').click();
  await page.locator("#send-panel").waitFor({ state: "visible" });
  if (screenshotDirectory) {
    await page.screenshot({ path: path.join(screenshotDirectory, walletOnly ? "store-send.png" : "desktop-send.png") });
  }

  await page.locator('[data-panel="activity-panel"]').click();
  await page.locator("#activity-list").waitFor({ state: "visible" });
  await page.waitForFunction(() => !document.querySelector("#activity-list")?.textContent?.includes("load wallet history"));
  assert.ok(!/networkerror|failed to fetch/i.test(await page.locator("body").innerText()));
  await assertDesktopShellVisible(page);

  if (walletOnly) {
    assert.equal(await page.locator('[data-panel="miner-panel"]').count(), 0);
    assert.equal(await page.locator("#miner-panel").count(), 0);
  } else {
    await page.locator('[data-panel="miner-panel"]').click();
    await page.locator("#miner-workers").fill("1");
    await page.locator("#start-miner").click();
    await page.waitForFunction(() => document.querySelector("#miner-state")?.textContent?.trim() !== "Stopped");
    await page.waitForTimeout(2000);
    await page.locator("#stop-miner").click();
    await page.waitForFunction(() => document.querySelector("#miner-state")?.textContent?.trim() === "Stopped");
    await assertDesktopShellVisible(page);
  }

  if (screenshotDirectory) {
    await page.screenshot({ path: path.join(screenshotDirectory, walletOnly ? "store-wallet.png" : "desktop-wallet.png") });
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
