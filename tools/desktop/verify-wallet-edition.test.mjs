import assert from "node:assert/strict";
import test from "node:test";

import { assertWalletEditionArtifact } from "./verify-wallet-edition.mjs";

test("wallet edition artifact gate accepts a wallet-only core", () => {
  assert.doesNotThrow(() => assertWalletEditionArtifact({
    symbols: "00401000 T main.runWalletDistribution\n00402000 T main.(*appService).Status",
    bytes: Buffer.from("BTC09 Wallet wallet edition /api/v1/status", "utf8"),
  }));
});

test("wallet edition artifact gate rejects linked mining code and UI", () => {
  for (const fixture of [
    { symbols: "00401000 T main.(*appService).StartMiner", bytes: Buffer.alloc(0) },
    { symbols: "00401000 T github.com/krutftw/bitcoin09/pool.(*RemoteClient).MineWork", bytes: Buffer.alloc(0) },
    { symbols: "", bytes: Buffer.from("POST /api/v1/miner/start", "utf8") },
    { symbols: "", bytes: Buffer.from("Start mining", "utf8") },
    { symbols: "", bytes: Buffer.from("BTC09 miner help report", "utf8") },
    { symbols: "", bytes: Buffer.from("Open solo mining coordinator URL", "utf8") },
  ]) {
    assert.throws(() => assertWalletEditionArtifact(fixture), /mining code or interface/i);
  }
});

test("wallet edition artifact gate rejects production demo controls", () => {
  for (const marker of ["?demo=", "navigator.webdriver", "demoScreen"]) {
    assert.throws(
      () => assertWalletEditionArtifact({ symbols: "", bytes: Buffer.from(marker, "utf8") }),
      /demo fixture/i,
    );
  }
});
