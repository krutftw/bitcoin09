"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const helperPath = path.resolve(__dirname, "../../desktop/assets/network.js");
const helperExists = fs.existsSync(helperPath);
const network = helperExists ? require(helperPath) : {};

test("desktop wallet includes a testable network helper", () => {
  assert.equal(helperExists, true, "desktop/assets/network.js is missing");
  assert.equal(typeof network.request, "function", "network.request is missing");
});

test("a transient GET failure is retried once", async () => {
  assert.equal(typeof network.request, "function", "network.request is missing");
  let attempts = 0;
  const result = await network.request("/api/v1/activity", {}, {
    fetch: async () => {
      attempts += 1;
      if (attempts === 1) throw new TypeError("NetworkError when attempting to fetch resource.");
      return response({ ok: true, data: { items: ["payment"] } });
    },
    wait: async () => {},
  });

  assert.deepEqual(result, { items: ["payment"] });
  assert.equal(attempts, 2);
});

test("a failed POST is not retried and gets human wording", async () => {
  assert.equal(typeof network.request, "function", "network.request is missing");
  let attempts = 0;

  await assert.rejects(
    network.request("/api/v1/miner/start", { method: "POST", body: "{}" }, {
      csrf: () => "csrf-token",
      fetch: async () => {
        attempts += 1;
        throw new TypeError("Failed to fetch");
      },
      wait: async () => {},
    }),
    (error) => {
      assert.equal(error.code, "wallet_unreachable");
      assert.equal(error.message, "BTC09 Wallet lost contact with the app. Make sure the app is still running, then try again.");
      return true;
    },
  );
  assert.equal(attempts, 1);
});

test("server errors and unreadable responses stay understandable", async () => {
  assert.equal(typeof network.request, "function", "network.request is missing");

  await assert.rejects(
    network.request("/api/v1/activity", {}, {
      fetch: async () => response({ ok: false, error: { code: "gateway_unavailable", message: "Activity is temporarily unavailable." } }, 503),
      wait: async () => {},
    }),
    (error) => error.code === "gateway_unavailable" && error.message === "Activity is temporarily unavailable.",
  );

  await assert.rejects(
    network.request("/api/v1/activity", {}, {
      fetch: async () => ({ ok: true, json: async () => { throw new SyntaxError("bad JSON"); } }),
      wait: async () => {},
    }),
    (error) => error.code === "invalid_response" && error.message === "BTC09 Wallet received an unreadable response. Try again.",
  );
});

test("POST requests include the current CSRF value", async () => {
  assert.equal(typeof network.request, "function", "network.request is missing");
  let captured;
  await network.request("/api/v1/miner/stop", { method: "POST", body: "{}", headers: { "X-Test": "kept" } }, {
    csrf: () => "csrf-token",
    fetch: async (_path, init) => {
      captured = init;
      return response({ ok: true, data: { state: "stopped" } });
    },
    wait: async () => {},
  });

  assert.equal(captured.credentials, "same-origin");
  assert.equal(captured.headers["Content-Type"], "application/json");
  assert.equal(captured.headers["X-BTC09-CSRF"], "csrf-token");
  assert.equal(captured.headers["X-Test"], "kept");
});

function response(payload, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => payload,
  };
}
