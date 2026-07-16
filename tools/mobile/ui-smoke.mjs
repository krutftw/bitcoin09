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
const cases = [
  ["onboarding", 390, 844], ["locked", 390, 844], ["home", 390, 844],
  ["receive", 390, 844], ["send", 390, 844], ["review", 390, 844],
  ["activity", 390, 844], ["settings", 390, 844], ["backup", 390, 844],
  ["home-small", 360, 740],
];

try {
  for (const [name, width, height] of cases) {
    const demo = name === "home-small" ? "home" : name;
    const page = await browser.newPage({ viewport: { width, height }, deviceScaleFactor: 1 });
    const failures = [];
    page.on("pageerror", (error) => failures.push(error.message));
    await page.goto(`http://127.0.0.1:${address.port}/mobile.html?demo=${demo}`, { waitUntil: "networkidle" });
    await page.locator(`[data-screen="${demo === "locked" ? "unlock" : demo}"]:visible`).waitFor();
    const metrics = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
      visibleTargets: [...document.querySelectorAll("button:not([hidden]), input:not([hidden]), textarea:not([hidden]), summary")]
        .filter((node) => node.getClientRects().length && !node.closest("[hidden]"))
        .filter((node) => node.type !== "checkbox" || (node.closest("label")?.getBoundingClientRect().height || 0) < 40)
        .map((node) => ({ label: node.getAttribute("aria-label") || node.textContent?.trim().slice(0, 40), height: node.getBoundingClientRect().height }))
        .filter((target) => target.height < 40),
    }));
    assert.equal(failures.length, 0, `${name} raised: ${failures.join("; ")}`);
    assert.ok(metrics.bodyWidth <= metrics.viewportWidth, `${name} overflows by ${metrics.bodyWidth - metrics.viewportWidth}px`);
    assert.deepEqual(metrics.visibleTargets, [], `${name} has undersized controls`);
    await page.screenshot({ path: path.join(output, `${name}-${width}x${height}.png`), fullPage: true });
    await page.close();
  }

  const flow = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await flow.goto(`http://127.0.0.1:${address.port}/mobile.html?demo=home`, { waitUntil: "networkidle" });
  await flow.getByRole("button", { name: "Send" }).click();
  await flow.locator("#send-address").fill("4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn");
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
  assert.equal(await flow.locator("#receive-address").textContent(), "4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn");
  await flow.goBack();
  await flow.locator("#home-screen:visible").waitFor();
  await flow.close();

  const firstRun = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await firstRun.goto(`http://127.0.0.1:${address.port}/mobile.html?demo=onboarding`, { waitUntil: "networkidle" });
  await firstRun.getByRole("button", { name: "Create a new wallet" }).click();
  await firstRun.locator("#backup-screen:visible").waitFor();
  assert.equal(await firstRun.locator("#recovery-words li").count(), 24);
  await firstRun.locator("#backup-confirmed").check();
  await firstRun.getByRole("button", { name: "Continue to wallet" }).click();
  await firstRun.locator("#home-screen:visible").waitFor();
  await firstRun.close();
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

process.stdout.write(`Mobile UI smoke passed. Screenshots: ${output}\n`);
