import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const source = path.join(root, "walletapp", "src");

test("mobile wallet has ordinary app screens without terminal language", async () => {
  const html = await readFile(path.join(source, "mobile.html"), "utf8");
  for (const id of [
    "onboarding-screen", "unlock-screen", "backup-screen", "home-screen", "receive-screen",
    "send-screen", "review-screen", "activity-screen", "settings-screen", "mobile-nav",
  ]) {
    assert.match(html, new RegExp(`id=["']${id}["']`));
  }
  for (const text of ["Receive", "Send", "Recent activity", "Recovery words", "No account. No terminal."]) {
    assert.match(html, new RegExp(text.replace(".", "\\.")));
  }
  assert.match(html, /id=["']scan-address["'][^>]*>Scan QR</);
  assert.doesNotMatch(html, /command line|CLI|localhost|127\.0\.0\.1|datadir|RPC/i);
});

test("mobile adapter uses only the wallet plugin and locks on background", async () => {
  const script = await readFile(path.join(source, "mobile.js"), "utf8");
  assert.match(script, /plugin:wallet-core\|/);
  assert.match(script, /visibilitychange/);
  assert.match(script, /history\.pushState/);
  assert.match(script, /history\.replaceState/);
  assert.match(script, /popstate/);
  assert.match(script, /call\("lock"\)/);
  assert.match(script, /BigInt/);
  assert.match(script, /navigator\.share/);
  assert.match(script, /__TAURI__\?\.barcodeScanner/);
  assert.match(script, /checkPermissions\(\)/);
  assert.match(script, /requestPermissions\(\)/);
  assert.match(script, /formats:\s*\[barcodeScanner\.Format\.QRCode\]/);
  assert.match(script, /cameraDirection:\s*["']back["']/);
  assert.doesNotMatch(script, /fetch\(|XMLHttpRequest|WebSocket|localStorage|sessionStorage/);
  assert.doesNotMatch(script, /setInterval|enumerateDevices|getUserMedia/);
  assert.doesNotMatch(script, /console\.(log|error)|recovery_phrase.*JSON\.stringify/i);
  assert.doesNotMatch(script, /innerHTML/);
});

test("mobile styling keeps controls touch sized and headings restrained", async () => {
  const css = await readFile(path.join(source, "mobile.css"), "utf8");
  assert.match(css, /min-height:\s*48px/);
  assert.match(css, /font-size:\s*clamp\(26px,[^;]+32px\)/);
  assert.match(css, /max-width:\s*520px/);
  assert.match(css, /env\(safe-area-inset-bottom\)/);
  assert.match(css, /prefers-reduced-motion/);
});

test("Android and iPhone configs use the mobile page and never bundle the miner", async () => {
  for (const platform of ["android", "ios"]) {
    const config = JSON.parse(await readFile(path.join(root, "walletapp", "src-tauri", `tauri.${platform}.conf.json`), "utf8"));
    assert.equal(config.app.windows[0].url, "mobile.html");
    assert.deepEqual(config.bundle.externalBin, []);
    assert.doesNotMatch(config.bundle.longDescription, /miner|mining|terminal|command line/i);
    assert.match(config.build.beforeBuildCommand, new RegExp(`prepare-core\\.mjs ${platform}`));
  }
});
