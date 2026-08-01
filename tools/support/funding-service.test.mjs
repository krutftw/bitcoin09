import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  createFundingService,
  mergeProviderPayment,
  summarizeFunding,
} from "./funding-service.mjs";
import { supporterTierFor } from "./supporter-tiers.mjs";

test("supporter tiers use the donor's cumulative confirmed USD total", () => {
  assert.equal(supporterTierFor(4.99), null);
  assert.equal(supporterTierFor(5).key, "supporter");
  assert.equal(supporterTierFor(25).key, "backer");
  assert.equal(supporterTierFor(100).key, "builder");
  assert.equal(supporterTierFor(250).key, "core_supporter");
});

test("funding summary counts only finished BTC09 USD payments", () => {
  const summary = summarizeFunding({
    updated_at: "2026-08-01T00:00:00Z",
    payments: {
      good: { order_id: "btc09-support-one", payment_status: "finished", price_amount: 12.345, price_currency: "usd" },
      other: { order_id: "copybot-one", payment_status: "finished", price_amount: 500, price_currency: "usd" },
      open: { order_id: "btc09-support-two", payment_status: "confirming", price_amount: 30, price_currency: "usd" },
      foreign: { order_id: "btc09-support-three", payment_status: "finished", price_amount: 40, price_currency: "eur" },
    },
  });
  assert.equal(summary.cash_received_usd, 12.35);
  assert.equal(summary.confirmed_payments, 1);
  assert.equal(summary.cash_remaining_usd, 3886.65);
});

test("provider updates cannot cross payment or order ids", () => {
  const record = { payment_id: "1234", order_id: "btc09-support-test", payment_status: "waiting" };
  assert.throws(() => mergeProviderPayment(record, { payment_id: "9999" }), /payment id mismatch/);
  assert.throws(() => mergeProviderPayment(record, { payment_id: "1234", order_id: "copybot" }), /order id mismatch/);
  assert.equal(mergeProviderPayment(record, {
    payment_id: "1234",
    order_id: "btc09-support-test",
    payment_status: "finished",
    pay_amount: "0.1",
  }, "2026-08-01T00:00:00Z").payment_status, "finished");
});

test("service creates, persists, refreshes, and totals a BTC09-only payment", async () => {
  const root = await mkdtemp(join(tmpdir(), "btc09-funding-test-"));
  const statePath = join(root, "payments.json");
  const calls = [];
  let createdOrderId = null;
  const fetchImpl = async (url, options = {}) => {
    calls.push({ url, method: options.method ?? "GET" });
    if (url.endsWith("/merchant/coins")) {
      return new Response(JSON.stringify({ selectedCurrencies: ["BTC", "eth"] }), { status: 200 });
    }
    if (url.endsWith("/payment") && options.method === "POST") {
      const body = JSON.parse(options.body);
      createdOrderId = body.order_id;
      return new Response(JSON.stringify({
        payment_id: 123456,
        payment_status: "waiting",
        pay_address: "bc1qtest",
        pay_amount: 0.0001,
        pay_currency: body.pay_currency,
        order_id: body.order_id,
        created_at: "2026-08-01T00:00:00Z",
      }), { status: 201 });
    }
    if (url.endsWith("/payment/123456")) {
      return new Response(JSON.stringify({
        payment_id: 123456,
        payment_status: "finished",
        order_id: createdOrderId,
        price_amount: 25,
        price_currency: "usd",
        pay_amount: 0.0001,
        pay_currency: "btc",
        updated_at: "2026-08-01T00:02:00Z",
      }), { status: 200 });
    }
    throw new Error(`unexpected request ${url}`);
  };

  try {
    const service = await createFundingService({
      apiKey: "test-key",
      statePath,
      fetchImpl,
      claimSecret: "claim-secret",
      clock: () => "2026-08-01T00:03:00Z",
    });
    const created = await service.createPayment({ amount_usd: 25, pay_currency: "btc" });
    assert.equal(created.payment.payment_status, "waiting");
    assert.match(created.token, /^[A-Za-z0-9_-]{32}$/);
    assert.match(createdOrderId, /^btc09-support-/);

    const server = createServer(service.handler);
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    try {
      const address = server.address();
      const path = `http://127.0.0.1:${address.port}/api/support/v1/payments/123456`;
      assert.equal((await fetch(`${path}?token=${created.token}`)).status, 404);
      const authorised = await fetch(path, { headers: { "X-BTC09-Payment-Token": created.token } });
      assert.equal(authorised.status, 200);
      assert.equal((await authorised.json()).payment.payment_status, "finished");

      const claimPath = `http://127.0.0.1:${address.port}/internal/support/v1/claims`;
      const claimBody = JSON.stringify({ claim_code: created.token, discord_user_id: "123456789012345678" });
      assert.equal((await fetch(claimPath, { method: "POST", body: claimBody })).status, 403);
      const claimedResponse = await fetch(claimPath, {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-BTC09-Claim-Secret": "claim-secret" },
        body: claimBody,
      });
      assert.equal(claimedResponse.status, 200);
      const claimed = await claimedResponse.json();
      assert.equal(claimed.total_confirmed_usd, 25);
      assert.equal(claimed.tier.key, "backer");

      const afterClaim = await fetch(path, { headers: { "X-BTC09-Payment-Token": created.token } });
      assert.equal((await afterClaim.json()).payment.claim_status, "claimed");
    } finally {
      await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    }

    await service.refreshPayment("123456", { force: true });
    const summary = summarizeFunding(service.getState());
    assert.equal(summary.cash_received_usd, 25);
    assert.equal(summary.confirmed_payments, 1);

    const saved = JSON.parse(await readFile(statePath, "utf8"));
    assert.equal(saved.payments["123456"].payment_status, "finished");
    assert.equal(saved.payments["123456"].claimed_by_discord_user_id, "123456789012345678");
    assert.equal(saved.payments["123456"].client_token, created.token);
    assert.equal(calls.filter((call) => call.url.endsWith("/payment")).length, 1);

    const repeated = await service.claimPayment({
      claim_code: created.token,
      discord_user_id: "123456789012345678",
    });
    assert.equal(repeated.tier.key, "backer");
    await assert.rejects(service.claimPayment({
      claim_code: created.token,
      discord_user_id: "999999999999999999",
    }), /already been used/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("separate finished payments add up for one Discord supporter", async () => {
  const root = await mkdtemp(join(tmpdir(), "btc09-funding-test-"));
  const statePath = join(root, "payments.json");
  let nextPaymentId = 800000;
  const orders = new Map();
  const fetchImpl = async (url, options = {}) => {
    if (url.endsWith("/merchant/coins")) {
      return new Response(JSON.stringify({ selectedCurrencies: ["btc"] }), { status: 200 });
    }
    if (url.endsWith("/payment") && options.method === "POST") {
      const body = JSON.parse(options.body);
      const paymentId = String(nextPaymentId++);
      orders.set(paymentId, body);
      return new Response(JSON.stringify({
        payment_id: paymentId,
        payment_status: "waiting",
        pay_address: `bc1q${paymentId}`,
        pay_amount: 0.001,
        pay_currency: "btc",
        order_id: body.order_id,
      }), { status: 201 });
    }
    const paymentId = url.split("/").at(-1);
    if (orders.has(paymentId)) {
      const order = orders.get(paymentId);
      return new Response(JSON.stringify({
        payment_id: paymentId,
        payment_status: "finished",
        order_id: order.order_id,
        price_amount: order.price_amount,
        price_currency: "usd",
        pay_amount: 0.001,
        pay_currency: "btc",
      }), { status: 200 });
    }
    throw new Error(`unexpected request ${url}`);
  };

  try {
    const service = await createFundingService({
      apiKey: "test-key",
      statePath,
      fetchImpl,
      claimSecret: "claim-secret",
    });
    const first = await service.createPayment({ amount_usd: 10, pay_currency: "btc" });
    const firstClaim = await service.claimPayment({
      claim_code: first.token,
      discord_user_id: "123456789012345678",
    });
    assert.equal(firstClaim.tier.key, "supporter");

    const second = await service.createPayment({ amount_usd: 20, pay_currency: "btc" });
    const secondClaim = await service.claimPayment({
      claim_code: second.token,
      discord_user_id: "123456789012345678",
    });
    assert.equal(secondClaim.total_confirmed_usd, 30);
    assert.equal(secondClaim.tier.key, "backer");
    assert.ok(Object.values(service.getState().payments).every((record) =>
      record.claimed_role_name === "🤝 Backer"
    ));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("service rejects an amount outside the published range", async () => {
  const root = await mkdtemp(join(tmpdir(), "btc09-funding-test-"));
  try {
    const service = await createFundingService({
      apiKey: "test-key",
      statePath: join(root, "payments.json"),
      fetchImpl: async () => {
        throw new Error("provider must not be called");
      },
    });
    await assert.rejects(service.createPayment({ amount_usd: 1, pay_currency: "btc" }), /between US\$5/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("service stops creating payments after the cash target is reached", async () => {
  const root = await mkdtemp(join(tmpdir(), "btc09-funding-test-"));
  try {
    const service = await createFundingService({
      apiKey: "test-key",
      statePath: join(root, "payments.json"),
      cashTargetUsd: 0,
      fetchImpl: async () => {
        throw new Error("provider must not be called");
      },
    });
    await assert.rejects(service.createPayment({ amount_usd: 25, pay_currency: "btc" }), /target has been reached/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
