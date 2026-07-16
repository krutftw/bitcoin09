import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const blockedSymbolPatterns = [
  /main\.\(\*appService\)\.(?:StartMiner|StopMiner|runMiner|observeMinerEvent)\b/,
  /github\.com\/krutftw\/bitcoin09\/pool\./,
];

const blockedContentMarkers = [
  "/api/v1/miner/",
  "Start mining",
  "BTC09 miner help report",
  "Open solo mining coordinator",
  "mine-pool",
];

const blockedDemoMarkers = ["?demo=", "navigator.webdriver", "demoScreen"];

export function assertWalletEditionArtifact({ symbols, bytes }) {
  const symbolText = String(symbols || "");
  for (const pattern of blockedSymbolPatterns) {
    if (pattern.test(symbolText)) {
      throw new Error("The wallet edition still contains mining code or interface symbols.");
    }
  }

  const binaryText = Buffer.isBuffer(bytes) ? bytes.toString("latin1") : String(bytes || "");
  for (const marker of blockedContentMarkers) {
    if (binaryText.includes(marker)) {
      throw new Error("The wallet edition still contains mining code or interface content.");
    }
  }
  for (const marker of blockedDemoMarkers) {
    if (binaryText.includes(marker)) {
      throw new Error("The wallet edition still contains a production demo fixture or control.");
    }
  }
}

export function verifyWalletEditionBinary(binaryPath) {
  const resolved = path.resolve(binaryPath);
  const result = spawnSync("go", ["tool", "nm", resolved], {
    encoding: "utf8",
    windowsHide: true,
    maxBuffer: 64 * 1024 * 1024,
  });
  if (result.status !== 0) {
    throw new Error(result.stderr?.trim() || "The wallet edition symbol table could not be inspected.");
  }
  assertWalletEditionArtifact({ symbols: result.stdout, bytes: readFileSync(resolved) });
  return resolved;
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  if (process.argv.length !== 3) {
    throw new Error("Usage: node verify-wallet-edition.mjs PATH_TO_BTC09_CORE");
  }
  const verified = verifyWalletEditionBinary(process.argv[2]);
  process.stdout.write(`Verified wallet-only BTC09 Core: ${verified}\n`);
}
