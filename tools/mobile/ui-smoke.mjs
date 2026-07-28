import assert from "node:assert/strict";
import { createReadStream } from "node:fs";
import { mkdir, rm } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const source = path.join(root, "walletapp", "src");
const output = path.join(tmpdir(), "btc09-mobile-ui");
const types = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
]);

await rm(output, { recursive: true, force: true });
await mkdir(output, { recursive: true });

const server = createServer((request, response) => {
  const requestPath = request.url?.split("?", 1)[0] || "/";
  const relative = requestPath === "/" ? "mobile.html" : requestPath.slice(1);
  const resolved = path.resolve(source, relative);
  if (!resolved.startsWith(`${source}${path.sep}`) && resolved !== path.join(source, "mobile.html")) {
    response.writeHead(404).end();
    return;
  }
  response.setHeader("Content-Type", types.get(path.extname(resolved)) || "application/octet-stream");
  createReadStream(resolved).on("error", () => response.writeHead(404).end()).pipe(response);
});

await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const address = server.address();
const browser = await chromium.launch({ headless: true });
const testAddress = "4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn";
const testPhrase = "anchor begin canvas daring elder fabric globe harbor ivory jungle kitten lunar maple noble olive picnic quiet river solar timber uncover velvet willow youth";
const testStatus = {
  wallet_state: "ready", sync_state: "connected", balance_available: true, send_available: true,
  balance_units: 12845000000, immature_units: 0, height: 8841, address: testAddress,
};
const testItems = [
  { txid: "5f0c5bf4201a76f3d8a3b0e870424adb90cc6d03969948128ce233be67891f08", kind: "received", status: "confirmed", net_units: 2500000000, confirmations: 38 },
  { txid: "0a63171dd9b329012ba3aa31e6b8ce56cd799a16858aa53fd3bab8e46fd603f1", kind: "sent", status: "confirmed", net_units: -420000000, confirmations: 112 },
  { txid: "d4c7c9a6c2f164849df52db7235c05238baa828a744b97a1c8696139122cfbad", kind: "received", status: "immature", net_units: 5000000000, confirmations: 9, blocks_until_mature: 91 },
];
const testQR = `data:image/svg+xml;base64,${Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 21 21"><rect width="21" height="21" fill="white"/><path d="M2 2h5v5H2zm12 0h5v5h-5zM2 14h5v5H2zm7-5h3v3H9zm5 4h5v2h-5zM9 16h3v3H9z" fill="#171713"/></svg>').toString("base64")}`;

async function installTestBridge(page, initialWalletState = "ready") {
  await page.addInitScript(({ initialWalletState: initial, address: walletAddress, phrase, status, items, qr }) => {
    let walletState = initial;
    const currentStatus = () => {
      if (walletState === "missing") {
        return { wallet_state: "missing", sync_state: "locked", balance_available: false, send_available: false, needs_unlock: false };
      }
      if (walletState === "locked") {
        return { ...status, wallet_state: "locked", sync_state: "locked", balance_available: false, send_available: false, needs_unlock: true };
      }
      return status;
    };
    const invoke = async (name, args = {}) => {
      const command = String(name).split("|").pop();
      const payload = args.payload || {};
      if (command === "status") return JSON.stringify(currentStatus());
      if (command === "activity") return JSON.stringify({ items, height: status.height });
      if (command === "receive") return JSON.stringify({ address: walletAddress, qr_data_url: qr });
      if (command === "recovery_phrase") return phrase;
      if (command === "create_wallet") {
        walletState = "ready";
        return JSON.stringify({ address: walletAddress, recovery_phrase: phrase });
      }
      if (command === "restore_wallet" || command === "unlock") {
        walletState = "ready";
        return JSON.stringify(status);
      }
      if (command === "lock") {
        if (walletState !== "missing") walletState = "locked";
        return "{}";
      }
      if (command === "preview_send") {
        return JSON.stringify({
          pending_id: "preview", destination: payload.destination, amount_units: 125000000,
          fee_units: 10000, total_units: 125010000, confirmation_code: "09A4C2",
        });
      }
      if (command === "confirm_send") return JSON.stringify({ txid: "1".repeat(64), status: "submitted" });
      return "{}";
    };
    window.__TAURI__ = {
      core: { invoke },
      barcodeScanner: {
        Format: { QRCode: "QR_CODE" },
        checkPermissions: async () => "prompt",
        requestPermissions: async () => "granted",
        scan: async (options) => {
          window.__btc09ScannerOptions = options;
          return { content: walletAddress, format: "QR_CODE", bounds: {} };
        },
      },
    };
  }, { initialWalletState, address: testAddress, phrase: testPhrase, status: testStatus, items: testItems, qr: testQR });
}

const cases = [
  { name: "onboarding", wallet: "missing", width: 390, height: 844 },
  { name: "locked", wallet: "locked", width: 390, height: 844 },
  { name: "home", wallet: "ready", width: 390, height: 844 },
  { name: "receive", wallet: "ready", width: 390, height: 844 },
  { name: "send", wallet: "ready", width: 390, height: 844 },
  { name: "review", wallet: "ready", width: 390, height: 844 },
  { name: "activity", wallet: "ready", width: 390, height: 844 },
  { name: "settings", wallet: "ready", width: 390, height: 844 },
  { name: "backup", wallet: "missing", width: 390, height: 844 },
  { name: "home-small", screen: "home", wallet: "ready", width: 360, height: 740 },
  { name: "onboarding-desktop", screen: "onboarding", wallet: "missing", width: 1180, height: 790 },
  { name: "home-desktop", screen: "home", wallet: "ready", width: 1180, height: 790 },
  { name: "receive-desktop", screen: "receive", wallet: "ready", width: 1180, height: 790 },
  { name: "send-desktop", screen: "send", wallet: "ready", width: 1180, height: 790 },
  { name: "review-desktop", screen: "review", wallet: "ready", width: 1180, height: 790 },
  { name: "activity-desktop", screen: "activity", wallet: "ready", width: 1180, height: 790 },
  { name: "settings-desktop", screen: "settings", wallet: "ready", width: 1180, height: 790 },
  { name: "backup-desktop", screen: "backup", wallet: "missing", width: 1180, height: 790 },
];

async function openCase(page, testCase) {
  const target = testCase.screen || testCase.name;
  await installTestBridge(page, testCase.wallet);
  await page.goto(`http://127.0.0.1:${address.port}/mobile.html`, { waitUntil: "networkidle" });
  if (target === "onboarding") return "onboarding";
  if (target === "locked") return "unlock";
  if (target === "backup") {
    await page.locator('[data-screen="onboarding"]:visible').waitFor();
    await page.getByRole("button", { name: "Create a new wallet" }).click();
    return "backup";
  }
  await page.locator('[data-screen="home"]:visible').waitFor();
  if (target === "receive") await page.getByRole("button", { name: "Receive" }).click();
  if (target === "send" || target === "review") {
    await page.getByRole("button", { name: "Send" }).click();
  }
  if (target === "review") {
    await page.locator("#send-address").fill(testAddress);
    await page.locator("#send-amount").fill("1.25");
    await page.getByRole("button", { name: "Review payment" }).click();
  }
  if (target === "activity") await page.getByRole("button", { name: "Activity" }).click();
  if (target === "settings") await page.getByRole("button", { name: "Settings" }).click();
  return target;
}

async function waitForStableScreen(page, screen) {
  await page.locator(`[data-screen="${screen}"]:visible`).evaluate(async (node) => {
    const animations = node.getAnimations({ subtree: true });
    await Promise.all(animations.map((animation) => animation.finished.catch(() => undefined)));
  });
}

try {
  for (const testCase of cases) {
    const { name, width, height } = testCase;
    const page = await browser.newPage({ viewport: { width, height }, deviceScaleFactor: 1 });
    const failures = [];
    page.on("pageerror", (error) => failures.push(error.message));
    const screen = await openCase(page, testCase);
    await page.locator(`[data-screen="${screen}"]:visible`).waitFor();
    await waitForStableScreen(page, screen);
    const metrics = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
      appBarVisible: Boolean(document.querySelector("#app-bar:not([hidden])")?.getClientRects().length),
      navigationVisible: Boolean(document.querySelector("#mobile-nav:not([hidden])")?.getClientRects().length),
      visibleTargets: [...document.querySelectorAll("button:not([hidden]), input:not([hidden]), textarea:not([hidden]), summary")]
        .filter((node) => node.getClientRects().length && !node.closest("[hidden]"))
        .filter((node) => node.type !== "checkbox" || (node.closest("label")?.getBoundingClientRect().height || 0) < 40)
        .map((node) => ({ label: node.getAttribute("aria-label") || node.textContent?.trim().slice(0, 40), height: node.getBoundingClientRect().height }))
        .filter((target) => target.height < 40),
    }));
    assert.equal(failures.length, 0, `${name} raised: ${failures.join("; ")}`);
    assert.ok(metrics.bodyWidth <= metrics.viewportWidth, `${name} overflows by ${metrics.bodyWidth - metrics.viewportWidth}px`);
    assert.deepEqual(metrics.visibleTargets, [], `${name} has undersized controls`);
    if (width >= 760 && !["onboarding", "unlock", "backup"].includes(screen)) {
      assert.equal(metrics.appBarVisible, true, `${name} should keep wallet status visible in the desktop rail`);
      assert.equal(metrics.navigationVisible, true, `${name} should keep desktop navigation visible`);
    }
    await page.screenshot({ path: path.join(output, `${name}-${width}x${height}.png`), fullPage: true });
    await page.close();
  }

  const flow = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await installTestBridge(flow, "ready");
  await flow.goto(`http://127.0.0.1:${address.port}/mobile.html`, { waitUntil: "networkidle" });
  await flow.getByRole("button", { name: "Send" }).click();
  await flow.getByRole("button", { name: "Scan QR" }).click();
  assert.equal(await flow.locator("#send-address").inputValue(), testAddress);
  assert.deepEqual(await flow.evaluate(() => window.__btc09ScannerOptions), {
    cameraDirection: "back", formats: ["QR_CODE"],
  });
  await flow.locator("#send-amount").fill("1.25");
  await flow.getByRole("button", { name: "Review payment" }).click();
  await flow.locator("#review-screen:visible").waitFor();
  assert.equal(await flow.locator("#review-total").textContent(), "1.2501 09C");
  const reviewAddressMetrics = await flow.locator("#review-address").evaluate((node) => ({
    fontSize: Number.parseFloat(getComputedStyle(node).fontSize),
    height: node.getBoundingClientRect().height,
  }));
  assert.ok(reviewAddressMetrics.fontSize >= 13, "review address text is too small");
  assert.ok(reviewAddressMetrics.height >= 36, "review address should wrap into a readable block");
  await flow.getByRole("button", { name: "Confirm and send" }).click();
  await flow.locator("#home-screen:visible").waitFor();
  await flow.getByRole("button", { name: "Receive" }).click();
  await flow.locator("#receive-screen:visible img").waitFor();
  assert.equal(await flow.locator("#receive-address").textContent(), testAddress);
  await flow.goBack();
  await flow.locator("#home-screen:visible").waitFor();
  await flow.close();

  const firstRun = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await installTestBridge(firstRun, "missing");
  await firstRun.goto(`http://127.0.0.1:${address.port}/mobile.html`, { waitUntil: "networkidle" });
  await firstRun.getByRole("button", { name: "Create a new wallet" }).click();
  await firstRun.locator("#backup-screen:visible").waitFor();
  assert.equal(await firstRun.locator("#recovery-words li").count(), 24);
  await firstRun.locator("#backup-confirmed").check();
  await firstRun.getByRole("button", { name: "Continue to wallet" }).click();
  await firstRun.locator("#home-screen:visible").waitFor();
  await firstRun.close();

  const lifecycle = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await installTestBridge(lifecycle, "missing");
  await lifecycle.goto(`http://127.0.0.1:${address.port}/mobile.html`, { waitUntil: "networkidle" });
  await lifecycle.getByRole("button", { name: "I have recovery words" }).click();
  await lifecycle.locator("#restore-words").fill(testPhrase);
  await lifecycle.evaluate(() => {
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  assert.equal(await lifecycle.locator("#restore-words").inputValue(), "");
  await lifecycle.locator("#unlock-screen:visible").waitFor();
  await lifecycle.evaluate(() => {
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await lifecycle.locator("#onboarding-screen:visible").waitFor();
  assert.equal(await lifecycle.locator("#restore-count").textContent(), "0 of 24 words");
  await lifecycle.close();
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

process.stdout.write(`Mobile UI smoke passed. Screenshots: ${output}\n`);
