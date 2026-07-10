import assert from "node:assert/strict";
import test from "node:test";
import { GatewaySessionPolicy } from "./gateway-session.mjs";
import { DiscordGatewayWatcher, fetchGatewayWithRetry } from "./gateway-watcher.mjs";

function createPolicy(options = {}) {
  return new GatewaySessionPolicy({
    gatewayUrl: "wss://gateway.discord.gg",
    random: () => 0.5,
    retryBaseMs: 500,
    retryMaxMs: 2_000,
    invalidSessionMinMs: 1_000,
    invalidSessionMaxMs: 5_000,
    ...options,
  });
}

function ready(policy, sequence = 42) {
  policy.observe({
    op: 0,
    s: sequence,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
    },
  });
}

function createWatcher(options = {}) {
  const timers = new FakeTimers();
  const sockets = [];
  const logs = [];
  class FakeWebSocket {
    constructor(url) {
      this.url = url;
      this.sent = [];
      this.closed = null;
      this.listeners = new Map();
      sockets.push(this);
    }

    addEventListener(type, listener) {
      const listeners = this.listeners.get(type) ?? [];
      listeners.push(listener);
      this.listeners.set(type, listeners);
    }

    send(payload) {
      this.sent.push(JSON.parse(payload));
    }

    close(code, reason) {
      this.closed = { code, reason };
    }

    async emit(type, event = {}) {
      const results = (this.listeners.get(type) ?? []).map((listener) => listener(event));
      await Promise.all(results);
    }

    async packet(packet) {
      await this.emit("message", { data: JSON.stringify(packet) });
    }
  }

  const logger = {
    log: (...values) => logs.push(values.join(" ")),
    error: (...values) => logs.push(values.join(" ")),
  };
  const fatal = [];
  const dispatches = [];
  const watcher = new DiscordGatewayWatcher({
    gatewayUrl: "wss://gateway.discord.gg",
    token: "super-secret-token",
    platform: "linux",
    WebSocketCtor: FakeWebSocket,
    random: () => 0.25,
    setTimeoutFn: timers.setTimeout.bind(timers),
    clearTimeoutFn: timers.clearTimeout.bind(timers),
    logger,
    onFatal: (decision) => fatal.push(decision),
    onDispatch: async (packet) => dispatches.push(packet),
    ...options,
  });
  return { watcher, timers, sockets, logs, fatal, dispatches };
}

class FakeTimers {
  constructor() {
    this.nextId = 1;
    this.tasks = new Map();
  }

  setTimeout(callback, delayMs) {
    const id = this.nextId;
    this.nextId += 1;
    this.tasks.set(id, { callback, delayMs });
    return id;
  }

  clearTimeout(id) {
    this.tasks.delete(id);
  }

  delays() {
    return [...this.tasks.values()].map((task) => task.delayMs);
  }

  ids() {
    return [...this.tasks.keys()];
  }

  async runNext() {
    const next = this.tasks.entries().next();
    assert.equal(next.done, false, "expected a scheduled timer");
    const [id, task] = next.value;
    this.tasks.delete(id);
    await task.callback();
  }

  async runDelay(delayMs) {
    const entry = [...this.tasks.entries()].find(([, task]) => task.delayMs === delayMs);
    assert.notEqual(entry, undefined, `expected a ${delayMs}ms timer`);
    const [id, task] = entry;
    this.tasks.delete(id);
    await task.callback();
  }
}

test("READY caches the sequence and session values used by Resume", () => {
  const policy = createPolicy();
  ready(policy);

  assert.deepEqual(policy.nextConnection(), {
    mode: "resume",
    url: "wss://resume.discord.gg/?v=10&encoding=json",
  });
  assert.deepEqual(policy.handshake("token-value", "win32"), {
    op: 6,
    d: {
      token: "token-value",
      session_id: "session-123",
      seq: 42,
    },
  });

  policy.observe({ op: 0, s: 43, t: "MESSAGE_CREATE", d: {} });
  assert.equal(policy.handshake("token-value", "win32").d.seq, 43);
});

test("a fresh session identifies with the existing watcher intents", () => {
  const policy = createPolicy();

  assert.deepEqual(policy.nextConnection(), {
    mode: "identify",
    url: "wss://gateway.discord.gg/?v=10&encoding=json",
  });
  assert.deepEqual(policy.handshake("token-value", "linux"), {
    op: 2,
    d: {
      token: "token-value",
      intents: 1,
      properties: {
        os: "linux",
        browser: "bitcoin09-stats-bot",
        device: "bitcoin09-stats-bot",
      },
    },
  });
});

test("opcode 7 reconnects immediately and resumes an established session", () => {
  const policy = createPolicy();
  ready(policy);

  assert.deepEqual(policy.reconnectRequested(), {
    action: "reconnect",
    delayMs: 0,
    mode: "resume",
    reason: "gateway_reconnect",
  });
});

test("opcode 9 follows the resumable flag", () => {
  const resumable = createPolicy();
  ready(resumable);
  assert.deepEqual(resumable.invalidSession(true), {
    action: "reconnect",
    delayMs: 0,
    mode: "resume",
    reason: "invalid_session_resumable",
  });

  const invalid = createPolicy({ random: () => 0.25 });
  ready(invalid);
  assert.deepEqual(invalid.invalidSession(false), {
    action: "reconnect",
    delayMs: 2_000,
    mode: "identify",
    reason: "invalid_session_non_resumable",
  });
  assert.equal(invalid.sessionId, null);
  assert.equal(invalid.sequence, null);
  assert.equal(invalid.resumeGatewayUrl, null);
  assert.equal(invalid.handshake("token-value", "linux").op, 2);
});

test("invalid-session jitter remains inside its configured bounds", () => {
  const minimum = createPolicy({ random: () => -1 });
  ready(minimum);
  assert.equal(minimum.invalidSession(false).delayMs, 1_000);

  const maximum = createPolicy({ random: () => 2 });
  ready(maximum);
  assert.equal(maximum.invalidSession(false).delayMs, 5_000);
});

test("Hello jitters only the first heartbeat and missing ACK triggers resume", () => {
  const policy = createPolicy({ random: () => 0.25 });
  ready(policy, 70);

  assert.deepEqual(policy.hello(40_000), {
    firstDelayMs: 10_000,
    intervalMs: 40_000,
  });
  assert.deepEqual(policy.heartbeatDue(), {
    action: "send",
    payload: { op: 1, d: 70 },
  });
  assert.deepEqual(policy.heartbeatDue(), {
    action: "reconnect",
    delayMs: 0,
    mode: "resume",
    reason: "heartbeat_ack_timeout",
  });

  policy.heartbeatAcknowledged();
  assert.deepEqual(policy.heartbeatDue(), {
    action: "send",
    payload: { op: 1, d: 70 },
  });
});

test("server heartbeat requests receive the latest sequence immediately", () => {
  const policy = createPolicy();
  policy.observe({ op: 0, s: 9, t: "GUILD_CREATE", d: {} });

  assert.deepEqual(policy.heartbeatRequested(), { op: 1, d: 9 });
});

test("abnormal failures back off to a cap and READY or RESUMED resets retries", () => {
  const policy = createPolicy();

  assert.equal(policy.transportFailure("close_1006").delayMs, 0);
  assert.equal(policy.transportFailure("close_1006").delayMs, 500);
  assert.equal(policy.transportFailure("close_1006").delayMs, 1_000);
  assert.equal(policy.transportFailure("close_1006").delayMs, 2_000);
  assert.equal(policy.transportFailure("close_1006").delayMs, 2_000);

  policy.observe({
    op: 0,
    s: 100,
    t: "READY",
    d: {
      session_id: "new-session",
      resume_gateway_url: "wss://resume.discord.gg",
    },
  });
  assert.equal(policy.transportFailure("close_1006").delayMs, 0);

  policy.transportFailure("close_1006");
  policy.observe({ op: 0, s: 101, t: "RESUMED", d: {} });
  assert.equal(policy.transportFailure("close_1006").delayMs, 0);
});

test("close policy resumes, re-identifies, or exits according to the close code", () => {
  const resumable = createPolicy();
  ready(resumable);
  assert.deepEqual(resumable.closed(1006), {
    action: "reconnect",
    delayMs: 0,
    mode: "resume",
    reason: "close_1006",
  });

  for (const code of [1000, 1001, 4007, 4009]) {
    const fresh = createPolicy();
    ready(fresh);
    const decision = fresh.closed(code);
    assert.equal(decision.action, "reconnect");
    assert.equal(decision.mode, "identify");
    assert.equal(fresh.sessionId, null);
  }

  for (const code of [4004, 4010, 4011, 4012, 4013, 4014]) {
    const terminal = createPolicy();
    ready(terminal);
    assert.deepEqual(terminal.closed(code), {
      action: "exit",
      code,
      exitCode: 0,
      reason: `terminal_close_${code}`,
    });
  }
});

test("watcher identifies on Hello and jitters only its first heartbeat", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();

  assert.equal(sockets[0].url, "wss://gateway.discord.gg/?v=10&encoding=json");
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  assert.equal(sockets[0].sent[0].op, 2);
  assert.deepEqual(timers.delays().sort((a, b) => a - b), [10_000, 30_000]);
  await sockets[0].packet({
    op: 0,
    s: 1,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });
  assert.deepEqual(timers.delays(), [10_000]);

  await timers.runNext();
  assert.deepEqual(sockets[0].sent[1], { op: 1, d: 1 });
  assert.deepEqual(timers.delays(), [40_000]);

  await sockets[0].packet({ op: 11, d: null });
  await timers.runNext();
  assert.deepEqual(sockets[0].sent[2], { op: 1, d: 1 });
  assert.deepEqual(timers.delays(), [40_000]);
});

test("watcher reconnects when the Gateway does not send Hello before the deadline", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();

  assert.deepEqual(timers.delays(), [15_000]);
  await timers.runDelay(15_000);
  assert.deepEqual(sockets[0].closed, { code: 4000, reason: "reconnect" });
  assert.deepEqual(timers.delays(), [0]);
});

test("watcher reconnects when Identify receives no session response", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });

  assert.equal(timers.delays().includes(30_000), true);
  await timers.runDelay(30_000);
  assert.deepEqual(sockets[0].closed, { code: 4000, reason: "reconnect" });
  assert.deepEqual(timers.delays(), [0]);
});

test("watcher reconnects on opcode 7 and sends Resume after the next Hello", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  await sockets[0].packet({
    op: 0,
    s: 42,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });

  await sockets[0].packet({ op: 7, d: null });
  assert.deepEqual(sockets[0].closed, { code: 4000, reason: "reconnect" });
  assert.deepEqual(timers.delays(), [0]);
  await timers.runNext();

  assert.equal(sockets[1].url, "wss://resume.discord.gg/?v=10&encoding=json");
  await sockets[1].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  assert.deepEqual(sockets[1].sent[0], {
    op: 6,
    d: {
      token: "super-secret-token",
      session_id: "session-123",
      seq: 42,
    },
  });
  assert.equal(timers.delays().includes(30_000), true);
  await sockets[1].packet({ op: 0, s: 43, t: "RESUMED", d: {} });
  assert.equal(timers.delays().includes(30_000), false);
});

test("watcher ignores late packets from a socket already pending reconnect", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  await sockets[0].packet({
    op: 0,
    s: 42,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });
  await sockets[0].packet({ op: 7, d: null });

  const sentBeforeLatePacket = sockets[0].sent.length;
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 5_000 } });

  assert.equal(sockets[0].sent.length, sentBeforeLatePacket);
  assert.deepEqual(timers.delays(), [0]);
});

test("watcher clears an invalid session and identifies after bounded jitter", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  await sockets[0].packet({
    op: 0,
    s: 42,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });

  await sockets[0].packet({ op: 9, d: false });
  assert.deepEqual(timers.delays(), [2_000]);
  await timers.runNext();
  assert.equal(sockets[1].url, "wss://gateway.discord.gg/?v=10&encoding=json");

  await sockets[1].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  assert.equal(sockets[1].sent[0].op, 2);
});

test("watcher reconnects a zombie connection when a heartbeat ACK is missing", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  await sockets[0].packet({
    op: 0,
    s: 42,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });

  await timers.runNext();
  await timers.runNext();
  assert.deepEqual(sockets[0].closed, { code: 4000, reason: "reconnect" });
  assert.deepEqual(timers.delays(), [0]);
  await timers.runNext();
  assert.equal(sockets[1].url, "wss://resume.discord.gg/?v=10&encoding=json");
});

test("server-requested heartbeat resets the ACK deadline to a full interval", async () => {
  const { watcher, timers, sockets } = createWatcher();
  watcher.start();
  await sockets[0].packet({ op: 10, d: { heartbeat_interval: 40_000 } });
  await sockets[0].packet({
    op: 0,
    s: 42,
    t: "READY",
    d: {
      session_id: "session-123",
      resume_gateway_url: "wss://resume.discord.gg",
      user: { username: "btc09", discriminator: "0" },
    },
  });
  await timers.runNext();
  await sockets[0].packet({ op: 11, d: null });

  const regularHeartbeatTimer = timers.ids()[0];
  await sockets[0].packet({ op: 1, d: null });

  assert.notEqual(timers.ids()[0], regularHeartbeatTimer);
  assert.deepEqual(timers.delays(), [40_000]);
  await sockets[0].packet({ op: 11, d: null });
  await timers.runNext();
  assert.equal(sockets[0].closed, null);
});

test("watcher stops retrying only for terminal gateway close codes", async () => {
  const { watcher, timers, sockets, fatal } = createWatcher();
  watcher.start();

  await sockets[0].emit("close", { code: 4013, reason: "invalid intents" });
  assert.equal(timers.delays().length, 0);
  assert.deepEqual(fatal, [{
    action: "exit",
    code: 4013,
    exitCode: 0,
    reason: "terminal_close_4013",
  }]);
});

test("watcher forwards dispatches and never logs its token", async () => {
  const { watcher, sockets, dispatches, logs } = createWatcher();
  watcher.start();
  const interaction = {
    op: 0,
    s: 12,
    t: "INTERACTION_CREATE",
    d: { id: "interaction-1" },
  };

  await sockets[0].packet(interaction);
  assert.deepEqual(dispatches, [interaction]);
  assert.equal(logs.join("\n").includes("super-secret-token"), false);
});

test("gateway discovery retries transient startup failures with bounded backoff", async () => {
  let attempts = 0;
  const delays = [];
  const gateway = await fetchGatewayWithRetry(
    async () => {
      attempts += 1;
      if (attempts < 3) {
        const error = new Error("Discord unavailable");
        error.status = 503;
        throw error;
      }
      return { url: "wss://gateway.discord.gg" };
    },
    {
      sleepFn: async (delayMs) => delays.push(delayMs),
      logger: { error: () => {} },
      retryBaseMs: 500,
      retryMaxMs: 1_000,
    },
  );

  assert.deepEqual(gateway, { url: "wss://gateway.discord.gg" });
  assert.equal(attempts, 3);
  assert.deepEqual(delays, [0, 500]);
});

test("gateway discovery surfaces terminal startup authentication errors", async () => {
  const delays = [];
  const error = new Error("Unauthorized");
  error.status = 401;
  await assert.rejects(
    fetchGatewayWithRetry(
      async () => {
        throw error;
      },
      {
        sleepFn: async (delayMs) => delays.push(delayMs),
        logger: { error: () => {} },
      },
    ),
    (candidate) => candidate === error && candidate.exitCode === 0,
  );
  assert.deepEqual(delays, []);
});
