import assert from "node:assert/strict";
import test from "node:test";

import * as artifactGate from "./verify-artifact.mjs";

const { assertMobileArtifactEntries } = artifactGate;

test("mobile artifact gate accepts ordinary packaged wallet files", () => {
  assert.doesNotThrow(() => assertMobileArtifactEntries([
    { name: "assets/mobile.html", bytes: Buffer.from("BTC09 Wallet Receive Send") },
    { name: "lib/arm64-v8a/libbtc09_wallet_lib.so", bytes: Buffer.from([0, 1, 2, 3]) },
  ]));
});

test("mobile artifact gate rejects demo wallets and mining UI", () => {
  for (const marker of [
    "navigator.webdriver",
    "demoScreen",
    "?demo=",
    "anchor begin canvas daring elder fabric",
    "4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn",
    "/api/v1/miner/start",
    "Start mining",
  ]) {
    assert.throws(
      () => assertMobileArtifactEntries([{ name: "assets/mobile.js", bytes: Buffer.from(marker) }]),
      /forbidden demo or mining content/i,
    );
  }
});

test("mobile artifact gate rejects signing keys", () => {
  for (const name of ["upload.jks", "release.keystore", "identity.p12", "codesign.pfx"]) {
    assert.throws(
      () => assertMobileArtifactEntries([{ name, bytes: Buffer.from("key") }]),
      /signing material/i,
    );
  }
});

test("direct Android release gate requires a real APK signer", () => {
  assert.equal(typeof artifactGate.assertAndroidApkSignature, "function");
  assert.doesNotThrow(() => artifactGate.assertAndroidApkSignature({
    status: 0,
    stdout: "Signer #1 certificate SHA-256 digest: 7f:09",
    stderr: "",
  }));
  assert.throws(
    () => artifactGate.assertAndroidApkSignature({
      status: 1,
      stdout: "",
      stderr: "DOES NOT VERIFY",
    }),
    /not signed by a valid release certificate/i,
  );
});
