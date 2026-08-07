import { createHash, randomBytes, randomUUID, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { pathToFileURL } from "node:url";
import { publicSupporterTier, supporterTierFor } from "./supporter-tiers.mjs";

const PROVIDER_BASE = "https://api.nowpayments.io/v1";
const ORDER_PREFIX = "btc09-support-";
const OPEN_STATUSES = new Set(["waiting", "confirming", "confirmed", "sending", "partially_paid"]);
const FINAL_STATUSES = new Set(["finished", "failed", "refunded", "expired"]);
const MAX_BODY_BYTES = 4096;
// Keep in lockstep with docs/exchanges.json funding (StakeCube native listing).
const DEFAULT_CASH_TARGET_USD = 1000;
const DEFAULT_09C_TARGET_USD = 0;
const DEFAULT_MIN_USD = 5;
const DEFAULT_MAX_USD = 1000;

function finiteNumber(value, fallback = 0) {
  const number = Number(value);
  return Number.isFinite(number) ? number : fallback;
}

function money(value) {
  return Math.round((finiteNumber(value) + Number.EPSILON) * 100) / 100;
}

function nowIso() {
  return new Date().toISOString();
}

function sortedCurrencies(payload) {
  const source = Array.isArray(payload)
    ? payload
    : payload?.currencies ?? payload?.coins ?? payload?.selectedCurrencies ?? payload?.selected_currencies ?? [];
  return [...new Set(source.map((entry) => {
    if (typeof entry === "string") return entry;
    return entry?.currency ?? entry?.ticker ?? entry?.code ?? null;
  }).filter((entry) => typeof entry === "string" && /^[a-z0-9_-]{2,24}$/i.test(entry))
    .map((entry) => entry.toLowerCase()))].sort();
}

function safeEqual(left, right) {
  if (typeof left !== "string" || typeof right !== "string") return false;
  const leftHash = createHash("sha256").update(left).digest();
  const rightHash = createHash("sha256").update(right).digest();
  return timingSafeEqual(leftHash, rightHash);
}

function publicPayment(record) {
  const claimStatus = record.claimed_by_discord_user_id
    ? "claimed"
    : record.payment_status === "finished" ? "ready" : "pending";
  return {
    payment_id: record.payment_id,
    payment_status: record.payment_status,
    price_amount: record.price_amount,
    price_currency: record.price_currency,
    pay_amount: record.pay_amount,
    pay_currency: record.pay_currency,
    pay_address: record.pay_address,
    payin_extra_id: record.payin_extra_id,
    created_at: record.created_at,
    updated_at: record.updated_at,
    claim_status: claimStatus,
    claimed_role_name: record.claimed_role_name ?? null,
  };
}

function countsAsSupport(record) {
  return record?.payment_status === "finished" &&
    typeof record?.order_id === "string" &&
    record.order_id.startsWith(ORDER_PREFIX) &&
    String(record.price_currency).toLowerCase() === "usd";
}

export function summarizeFunding(state, {
  cashTargetUsd = DEFAULT_CASH_TARGET_USD,
  coinTargetUsd = DEFAULT_09C_TARGET_USD,
} = {}) {
  const records = Object.values(state?.payments ?? {});
  const finished = records.filter(countsAsSupport);
  const cashReceivedUsd = money(finished.reduce((total, record) => total + finiteNumber(record.price_amount), 0));
  return {
    schema_version: 1,
    provider: "NOWPayments",
    tracking: "finished BTC09 support payments created through this page",
    cash_target_usd: money(cashTargetUsd),
    cash_received_usd: cashReceivedUsd,
    cash_remaining_usd: money(Math.max(0, finiteNumber(cashTargetUsd) - cashReceivedUsd)),
    coin_liquidity_target_usd: money(coinTargetUsd),
    coin_liquidity_received_usd: 0,
    confirmed_payments: finished.length,
    updated_at: state?.updated_at ?? null,
  };
}

export function mergeProviderPayment(record, payload, checkedAt = nowIso()) {
  if (String(payload?.payment_id ?? "") !== String(record.payment_id)) {
    throw new Error("provider payment id mismatch");
  }
  if (payload?.order_id && payload.order_id !== record.order_id) {
    throw new Error("provider order id mismatch");
  }
  const status = String(payload?.payment_status ?? record.payment_status ?? "waiting").toLowerCase();
  if (!OPEN_STATUSES.has(status) && !FINAL_STATUSES.has(status)) {
    throw new Error("unsupported provider payment status");
  }
  return {
    ...record,
    payment_status: status,
    pay_amount: finiteNumber(payload?.pay_amount, record.pay_amount),
    actually_paid: finiteNumber(payload?.actually_paid, record.actually_paid),
    outcome_amount: finiteNumber(payload?.outcome_amount, record.outcome_amount),
    outcome_currency: payload?.outcome_currency ?? record.outcome_currency ?? null,
    updated_at: payload?.updated_at ?? checkedAt,
    last_provider_check_at: checkedAt,
  };
}

async function loadState(path) {
  try {
    const parsed = JSON.parse(await readFile(path, "utf8"));
    if (parsed?.schema_version !== 1 || typeof parsed?.payments !== "object" || Array.isArray(parsed.payments)) {
      throw new Error("unsupported funding state");
    }
    return parsed;
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
    return { schema_version: 1, updated_at: null, payments: {} };
  }
}

async function saveState(path, state) {
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const temporary = `${path}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`;
  await writeFile(temporary, `${JSON.stringify(state, null, 2)}\n`, { encoding: "utf8", mode: 0o600 });
  await rename(temporary, path);
}

async function readJsonBody(request) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) {
      const error = new Error("request body too large");
      error.statusCode = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  if (size === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    const error = new Error("invalid JSON body");
    error.statusCode = 400;
    throw error;
  }
}

function sendJson(response, statusCode, payload) {
  const body = `${JSON.stringify(payload)}\n`;
  response.writeHead(statusCode, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  response.end(body);
}

async function providerJson(fetchImpl, apiKey, path, options = {}) {
  const response = await fetchImpl(`${PROVIDER_BASE}${path}`, {
    ...options,
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      "x-api-key": apiKey,
      ...(options.headers ?? {}),
    },
    signal: options.signal ?? AbortSignal.timeout(12_000),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(`NOWPayments request failed (${response.status})`);
    error.statusCode = response.status >= 500 ? 503 : 400;
    error.providerStatus = response.status;
    error.providerCode = payload?.code ?? null;
    throw error;
  }
  return payload;
}

export async function createFundingService({
  apiKey,
  statePath,
  fetchImpl = fetch,
  cashTargetUsd = DEFAULT_CASH_TARGET_USD,
  coinTargetUsd = DEFAULT_09C_TARGET_USD,
  minUsd = DEFAULT_MIN_USD,
  maxUsd = DEFAULT_MAX_USD,
  claimSecret,
  clock = nowIso,
} = {}) {
  if (!apiKey) throw new Error("NOWPAYMENTS_API_KEY is required");
  if (!statePath) throw new Error("BTC09_SUPPORT_STATE is required");

  let state = await loadState(statePath);
  let mutation = Promise.resolve();
  let currencyCache = { values: [], expiresAt: 0 };

  async function updateState(updater) {
    const operation = mutation.then(async () => {
      const next = await updater(state);
      next.updated_at = clock();
      await saveState(statePath, next);
      state = next;
      return next;
    });
    mutation = operation.catch(() => {});
    return operation;
  }

  async function currencies() {
    if (currencyCache.expiresAt > Date.now() && currencyCache.values.length > 0) {
      return currencyCache.values;
    }
    let values = [];
    try {
      values = sortedCurrencies(await providerJson(fetchImpl, apiKey, "/merchant/coins"));
    } catch {
      values = [];
    }
    if (values.length === 0) {
      values = sortedCurrencies(await providerJson(fetchImpl, apiKey, "/currencies?fixed_rate=false"));
    }
    if (values.length === 0) throw new Error("NOWPayments returned no supported currencies");
    currencyCache = { values, expiresAt: Date.now() + 15 * 60 * 1000 };
    return values;
  }

  async function refreshPayment(paymentId, { force = false } = {}) {
    const record = state.payments[String(paymentId)];
    if (!record) return null;
    const lastCheck = Date.parse(record.last_provider_check_at ?? "");
    const recent = Number.isFinite(lastCheck) && Date.now() - lastCheck < 30_000;
    if (!force && recent) return record;
    const payload = await providerJson(fetchImpl, apiKey, `/payment/${encodeURIComponent(record.payment_id)}`);
    let merged = null;
    await updateState((current) => {
      const currentRecord = current.payments[String(record.payment_id)];
      if (!currentRecord) return current;
      merged = mergeProviderPayment(currentRecord, payload, clock());
      return {
        ...current,
        payments: { ...current.payments, [String(record.payment_id)]: merged },
      };
    });
    return merged;
  }

  async function refreshTrackedPayments() {
    const records = Object.values(state.payments);
    for (const record of records) {
      const checked = Date.parse(record.last_provider_check_at ?? "");
      const age = Number.isFinite(checked) ? Date.now() - checked : Infinity;
      const shouldRefresh = OPEN_STATUSES.has(record.payment_status) ||
        (record.payment_status === "finished" && age >= 6 * 60 * 60 * 1000);
      if (!shouldRefresh) continue;
      try {
        await refreshPayment(record.payment_id, { force: true });
      } catch (error) {
        console.error(`Funding payment refresh failed for ${record.payment_id}: ${error.message}`);
      }
    }
  }

  async function createPayment(body) {
    const amount = money(body?.amount_usd);
    const currency = String(body?.pay_currency ?? "").trim().toLowerCase();
    const remaining = summarizeFunding(state, { cashTargetUsd, coinTargetUsd }).cash_remaining_usd;
    if (remaining < finiteNumber(minUsd)) {
      const error = new Error("The current support target has been reached.");
      error.statusCode = 409;
      throw error;
    }
    if (amount < finiteNumber(minUsd) || amount > finiteNumber(maxUsd)) {
      const error = new Error(`amount must be between US$${money(minUsd)} and US$${money(maxUsd)}`);
      error.statusCode = 400;
      throw error;
    }
    if (amount > remaining) {
      const error = new Error(`amount cannot be more than the remaining US$${money(remaining)} target`);
      error.statusCode = 400;
      throw error;
    }
    const available = await currencies();
    if (!available.includes(currency)) {
      const error = new Error("unsupported payment currency");
      error.statusCode = 400;
      throw error;
    }

    const orderId = `${ORDER_PREFIX}${randomUUID()}`;
    let payload;
    try {
      payload = await providerJson(fetchImpl, apiKey, "/payment", {
        method: "POST",
        body: JSON.stringify({
          price_amount: amount,
          price_currency: "usd",
          pay_currency: currency,
          order_id: orderId,
          order_description: "BTC09 exchange support",
          is_fixed_rate: false,
          is_fee_paid_by_user: false,
        }),
      });
    } catch (error) {
      if (error?.providerStatus >= 400 && error?.providerStatus < 500) {
        const friendly = new Error("NOWPayments could not create this payment. Try a higher amount or a different coin.");
        friendly.statusCode = 400;
        throw friendly;
      }
      throw error;
    }
    if (!payload?.payment_id || !payload?.pay_address || payload?.order_id !== orderId) {
      throw new Error("NOWPayments returned an incomplete payment");
    }

    const clientToken = randomBytes(24).toString("base64url");
    const record = {
      payment_id: String(payload.payment_id),
      client_token: clientToken,
      order_id: orderId,
      payment_status: String(payload.payment_status ?? "waiting").toLowerCase(),
      price_amount: amount,
      price_currency: "usd",
      pay_amount: finiteNumber(payload.pay_amount),
      pay_currency: String(payload.pay_currency ?? currency).toLowerCase(),
      pay_address: String(payload.pay_address),
      payin_extra_id: payload.payin_extra_id ?? null,
      actually_paid: 0,
      outcome_amount: 0,
      outcome_currency: null,
      created_at: payload.created_at ?? clock(),
      updated_at: payload.updated_at ?? clock(),
      last_provider_check_at: clock(),
    };
    await updateState((current) => ({
      ...current,
      payments: { ...current.payments, [record.payment_id]: record },
    }));
    return { token: clientToken, payment: publicPayment(record) };
  }

  async function claimPayment(body) {
    const claimCode = String(body?.claim_code ?? "").trim();
    const discordUserId = String(body?.discord_user_id ?? "").trim();
    if (!/^[A-Za-z0-9_-]{32}$/.test(claimCode) || !/^\d{15,22}$/.test(discordUserId)) {
      const error = new Error("claim code is invalid");
      error.statusCode = 400;
      throw error;
    }

    let result = null;
    await updateState(async (current) => {
      const found = Object.values(current.payments).find((record) =>
        safeEqual(claimCode, record.client_token)
      );
      if (!found) {
        const error = new Error("claim code is invalid");
        error.statusCode = 404;
        throw error;
      }
      if (
        found.claimed_by_discord_user_id &&
        found.claimed_by_discord_user_id !== discordUserId
      ) {
        const error = new Error("claim code has already been used");
        error.statusCode = 409;
        throw error;
      }

      const currentRecord = found.claimed_by_discord_user_id === discordUserId && countsAsSupport(found)
        ? found
        : mergeProviderPayment(
          found,
          await providerJson(fetchImpl, apiKey, `/payment/${encodeURIComponent(found.payment_id)}`),
          clock(),
        );
      if (!countsAsSupport(currentRecord)) {
        const error = new Error("payment is not finished yet");
        error.statusCode = 409;
        throw error;
      }

      const payments = {
        ...current.payments,
        [String(currentRecord.payment_id)]: {
          ...currentRecord,
          claimed_by_discord_user_id: discordUserId,
          claimed_at: currentRecord.claimed_at ?? clock(),
        },
      };
      const total = money(Object.values(payments)
        .filter((record) => countsAsSupport(record) && record.claimed_by_discord_user_id === discordUserId)
        .reduce((sum, record) => sum + finiteNumber(record.price_amount), 0));
      const tier = supporterTierFor(total);
      if (!tier) {
        const error = new Error("confirmed support is below the minimum tier");
        error.statusCode = 409;
        throw error;
      }

      for (const [paymentId, record] of Object.entries(payments)) {
        if (record.claimed_by_discord_user_id !== discordUserId) continue;
        payments[paymentId] = {
          ...record,
          supporter_tier_key: tier.key,
          supporter_total_usd: total,
          claimed_role_name: tier.roleName,
        };
      }
      result = {
        claimed: true,
        payment_id: String(currentRecord.payment_id),
        payment_usd: money(currentRecord.price_amount),
        total_confirmed_usd: total,
        tier: publicSupporterTier(tier),
      };
      return { ...current, payments };
    });
    return result;
  }

  async function handler(request, response) {
    try {
      const url = new URL(request.url, "http://127.0.0.1");
      if (request.method === "GET" && url.pathname === "/healthz") {
        return sendJson(response, 200, { ok: true });
      }
      if (request.method === "GET" && url.pathname === "/api/support/v1/status") {
        return sendJson(response, 200, summarizeFunding(state, { cashTargetUsd, coinTargetUsd }));
      }
      if (request.method === "GET" && url.pathname === "/api/support/v1/currencies") {
        return sendJson(response, 200, { currencies: await currencies(), updated_at: clock() });
      }
      if (request.method === "POST" && url.pathname === "/api/support/v1/payments") {
        return sendJson(response, 201, await createPayment(await readJsonBody(request)));
      }
      if (request.method === "POST" && url.pathname === "/internal/support/v1/claims") {
        if (!claimSecret || !safeEqual(request.headers["x-btc09-claim-secret"], claimSecret)) {
          return sendJson(response, 403, { error: "forbidden" });
        }
        return sendJson(response, 200, await claimPayment(await readJsonBody(request)));
      }
      const match = url.pathname.match(/^\/api\/support\/v1\/payments\/([0-9]{4,32})$/);
      if (request.method === "GET" && match) {
        const record = state.payments[match[1]];
        const clientToken = request.headers["x-btc09-payment-token"];
        if (!record || !safeEqual(clientToken, record.client_token)) {
          return sendJson(response, 404, { error: "payment not found" });
        }
        const refreshed = await refreshPayment(match[1]);
        return sendJson(response, 200, { payment: publicPayment(refreshed) });
      }
      return sendJson(response, 404, { error: "not found" });
    } catch (error) {
      const statusCode = Number(error?.statusCode) || 500;
      if (statusCode >= 500) console.error(`Funding service request failed: ${error.message}`);
      return sendJson(response, statusCode, { error: statusCode >= 500 ? "payment service unavailable" : error.message });
    }
  }

  return {
    handler,
    currencies,
    createPayment,
    claimPayment,
    refreshPayment,
    refreshTrackedPayments,
    getState: () => state,
  };
}

export async function startFundingService(env = process.env) {
  const listen = env.BTC09_SUPPORT_LISTEN ?? "127.0.0.1:8032";
  const separator = listen.lastIndexOf(":");
  const host = listen.slice(0, separator);
  const port = Number(listen.slice(separator + 1));
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) throw new Error("invalid BTC09_SUPPORT_LISTEN");
  const service = await createFundingService({
    apiKey: env.NOWPAYMENTS_API_KEY,
    statePath: env.BTC09_SUPPORT_STATE ?? "/var/lib/btc09-support/payments.json",
    cashTargetUsd: finiteNumber(env.BTC09_SUPPORT_CASH_TARGET_USD, DEFAULT_CASH_TARGET_USD),
    coinTargetUsd: finiteNumber(env.BTC09_SUPPORT_09C_TARGET_USD, DEFAULT_09C_TARGET_USD),
    minUsd: finiteNumber(env.BTC09_SUPPORT_MIN_USD, DEFAULT_MIN_USD),
    maxUsd: finiteNumber(env.BTC09_SUPPORT_MAX_USD, DEFAULT_MAX_USD),
    claimSecret: env.BTC09_SUPPORT_CLAIM_SECRET,
  });
  const server = createServer(service.handler);
  server.requestTimeout = 15_000;
  server.headersTimeout = 10_000;
  server.keepAliveTimeout = 5_000;
  server.maxRequestsPerSocket = 100;
  server.listen(port, host, () => console.log(`BTC09 funding service listening on ${host}:${port}`));
  const timer = setInterval(() => service.refreshTrackedPayments(), 120_000);
  timer.unref();
  await service.refreshTrackedPayments();
  return { server, service, timer };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  startFundingService().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
