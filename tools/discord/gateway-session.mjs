const TERMINAL_CLOSE_CODES = new Set([4004, 4010, 4011, 4012, 4013, 4014]);
const FRESH_SESSION_CLOSE_CODES = new Set([1000, 1001, 4007, 4009]);

export class GatewaySessionPolicy {
  constructor({
    gatewayUrl,
    random = Math.random,
    retryBaseMs = 500,
    retryMaxMs = 15_000,
    invalidSessionMinMs = 1_000,
    invalidSessionMaxMs = 5_000,
  }) {
    if (!gatewayUrl) throw new Error("Discord gateway URL is required.");

    this.gatewayUrl = gatewayUrl;
    this.random = random;
    this.retryBaseMs = retryBaseMs;
    this.retryMaxMs = retryMaxMs;
    this.invalidSessionMinMs = invalidSessionMinMs;
    this.invalidSessionMaxMs = invalidSessionMaxMs;

    this.sequence = null;
    this.sessionId = null;
    this.resumeGatewayUrl = null;
    this.heartbeatAcked = true;
    this.retryAttempt = 0;
  }

  get canResume() {
    return this.sessionId != null &&
      this.sequence != null &&
      this.resumeGatewayUrl != null;
  }

  observe(packet) {
    if (packet.s != null) this.sequence = packet.s;

    if (packet.t === "READY") {
      this.sessionId = packet.d?.session_id ?? null;
      this.resumeGatewayUrl = packet.d?.resume_gateway_url ?? null;
      this.retryAttempt = 0;
    } else if (packet.t === "RESUMED") {
      this.retryAttempt = 0;
    }
  }

  nextConnection() {
    const mode = this.canResume ? "resume" : "identify";
    const baseUrl = mode === "resume" ? this.resumeGatewayUrl : this.gatewayUrl;
    return {
      mode,
      url: gatewayUrl(baseUrl),
    };
  }

  handshake(token, platform) {
    if (this.canResume) {
      return {
        op: 6,
        d: {
          token,
          session_id: this.sessionId,
          seq: this.sequence,
        },
      };
    }

    return {
      op: 2,
      d: {
        token,
        intents: 513,
        properties: {
          os: platform,
          browser: "bitcoin09-stats-bot",
          device: "bitcoin09-stats-bot",
        },
      },
    };
  }

  reconnectRequested() {
    return this.reconnect("gateway_reconnect", 0);
  }

  invalidSession(resumable) {
    if (resumable && this.canResume) {
      return this.reconnect("invalid_session_resumable", 0);
    }

    this.clearSession();
    return this.reconnect(
      "invalid_session_non_resumable",
      boundedJitter(
        this.invalidSessionMinMs,
        this.invalidSessionMaxMs,
        this.random(),
      ),
    );
  }

  hello(heartbeatInterval) {
    if (!Number.isFinite(heartbeatInterval) || heartbeatInterval <= 0) {
      throw new Error(`Invalid Discord heartbeat interval: ${heartbeatInterval}`);
    }

    this.heartbeatAcked = true;
    return {
      firstDelayMs: Math.floor(heartbeatInterval * normalizedRandom(this.random())),
      intervalMs: heartbeatInterval,
    };
  }

  heartbeatDue() {
    if (!this.heartbeatAcked) {
      return this.transportFailure("heartbeat_ack_timeout");
    }

    this.heartbeatAcked = false;
    return {
      action: "send",
      payload: this.heartbeatPayload(),
    };
  }

  heartbeatRequested() {
    if (!this.heartbeatAcked) return null;
    this.heartbeatAcked = false;
    return this.heartbeatPayload();
  }

  heartbeatAcknowledged() {
    this.heartbeatAcked = true;
  }

  closed(code) {
    if (TERMINAL_CLOSE_CODES.has(code)) {
      return {
        action: "exit",
        code,
        exitCode: 0,
        reason: `terminal_close_${code}`,
      };
    }

    if (FRESH_SESSION_CLOSE_CODES.has(code)) this.clearSession();
    return this.transportFailure(`close_${code}`);
  }

  transportFailure(reason) {
    const attempt = this.retryAttempt;
    this.retryAttempt += 1;

    const delayMs = attempt === 0
      ? 0
      : Math.min(this.retryBaseMs * (2 ** (attempt - 1)), this.retryMaxMs);
    return this.reconnect(reason, delayMs);
  }

  clearSession() {
    this.sequence = null;
    this.sessionId = null;
    this.resumeGatewayUrl = null;
    this.heartbeatAcked = true;
  }

  heartbeatPayload() {
    return { op: 1, d: this.sequence };
  }

  reconnect(reason, delayMs) {
    return {
      action: "reconnect",
      delayMs,
      mode: this.canResume ? "resume" : "identify",
      reason,
    };
  }
}

function gatewayUrl(baseUrl) {
  const url = new URL(baseUrl);
  url.searchParams.set("v", "10");
  url.searchParams.set("encoding", "json");
  return url.toString();
}

function boundedJitter(minimum, maximum, randomValue) {
  const low = Math.min(minimum, maximum);
  const high = Math.max(minimum, maximum);
  return Math.floor(low + ((high - low) * normalizedRandom(randomValue)));
}

function normalizedRandom(value) {
  if (!Number.isFinite(value)) return 0;
  return Math.max(0, Math.min(1, value));
}
