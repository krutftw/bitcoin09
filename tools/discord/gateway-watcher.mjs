import { GatewaySessionPolicy } from "./gateway-session.mjs";

export class DiscordGatewayWatcher {
  constructor({
    gatewayUrl,
    token,
    platform = process.platform,
    WebSocketCtor = globalThis.WebSocket,
    random = Math.random,
    setTimeoutFn = setTimeout,
    clearTimeoutFn = clearTimeout,
    logger = console,
    onDispatch = async () => {},
    onFatal = () => {},
    helloTimeoutMs = 15_000,
    sessionTimeoutMs = 30_000,
    policyOptions = {},
  }) {
    if (typeof WebSocketCtor !== "function") {
      throw new Error("This Node runtime does not provide WebSocket. Use Node 22+ or install a websocket client.");
    }
    if (!token) throw new Error("Discord bot token is required.");

    this.token = token;
    this.platform = platform;
    this.WebSocketCtor = WebSocketCtor;
    this.setTimeoutFn = setTimeoutFn;
    this.clearTimeoutFn = clearTimeoutFn;
    this.logger = logger;
    this.onDispatch = onDispatch;
    this.onFatal = onFatal;
    this.helloTimeoutMs = helloTimeoutMs;
    this.sessionTimeoutMs = sessionTimeoutMs;
    this.policy = new GatewaySessionPolicy({ gatewayUrl, random, ...policyOptions });

    this.context = null;
    this.retryTimer = null;
    this.started = false;
    this.stopped = false;
  }

  start() {
    if (this.started) return;
    this.started = true;
    this.connect();
  }

  stop() {
    this.stopped = true;
    if (this.retryTimer != null) {
      this.clearTimeoutFn(this.retryTimer);
      this.retryTimer = null;
    }

    if (!this.context) return;
    this.context.reconnectScheduled = true;
    this.clearHeartbeat(this.context);
    this.clearHandshakeDeadline(this.context);
    try {
      this.context.socket.close(1000, "shutdown");
    } catch {
      // The transport is already gone.
    }
  }

  connect() {
    if (this.stopped) return;
    this.retryTimer = null;
    const connection = this.policy.nextConnection();
    this.logger.log(`Connecting Discord gateway (${connection.mode}).`);

    let socket;
    try {
      socket = new this.WebSocketCtor(connection.url);
    } catch (error) {
      this.logger.error(`Gateway connect failed: ${errorMessage(error)}.`);
      this.scheduleConnect(this.policy.transportFailure("connect_error"));
      return;
    }

    const context = {
      socket,
      mode: connection.mode,
      heartbeatTimer: null,
      heartbeatInterval: null,
      handshakeTimer: null,
      reconnectScheduled: false,
    };
    this.context = context;

    socket.addEventListener("open", () => {
      if (this.isCurrent(context)) this.logger.log(`Gateway socket open (${context.mode}).`);
    });
    socket.addEventListener("message", (event) => this.handleMessage(context, event));
    socket.addEventListener("close", (event) => this.handleClose(context, event));
    socket.addEventListener("error", (event) => this.handleError(context, event));
    this.scheduleHandshakeDeadline(context, this.helloTimeoutMs, "hello_timeout");
  }

  async handleMessage(context, event) {
    if (!this.isCurrent(context)) return;

    let packet;
    try {
      packet = JSON.parse(event.data);
    } catch {
      this.logger.error("Gateway sent a non-JSON payload; ignoring it.");
      return;
    }

    this.policy.observe(packet);

    if (packet.op === 10) {
      let heartbeat;
      try {
        heartbeat = this.policy.hello(packet.d?.heartbeat_interval);
      } catch (error) {
        this.logger.error(`Gateway Hello rejected: ${errorMessage(error)}.`);
        this.scheduleReconnect(context, this.policy.transportFailure("invalid_hello"));
        return;
      }

      context.heartbeatInterval = heartbeat.intervalMs;
      this.scheduleHandshakeDeadline(
        context,
        this.sessionTimeoutMs,
        context.mode === "resume" ? "resume_timeout" : "identify_timeout",
      );
      this.scheduleHeartbeat(context, heartbeat.firstDelayMs);
      this.logger.log(
        `Gateway Hello: heartbeat=${heartbeat.intervalMs}ms first=${heartbeat.firstDelayMs}ms mode=${context.mode}.`,
      );
      this.send(context, this.policy.handshake(this.token, this.platform));
      return;
    }

    if (packet.op === 11) {
      this.policy.heartbeatAcknowledged();
      return;
    }

    if (packet.op === 1) {
      const heartbeat = this.policy.heartbeatRequested();
      if (heartbeat == null) {
        this.logger.error(
          `Gateway requested another heartbeat before ACK; reconnecting sequence=${stateValue(this.policy.sequence)} ` +
          `session=${this.policy.canResume ? "resumable" : "fresh"}.`,
        );
        this.scheduleReconnect(context, this.policy.transportFailure("heartbeat_ack_timeout"));
        return;
      }
      if (this.send(context, heartbeat) && context.heartbeatInterval != null) {
        this.scheduleHeartbeat(context, context.heartbeatInterval);
      }
      return;
    }

    if (packet.op === 7) {
      this.scheduleReconnect(context, this.policy.reconnectRequested());
      return;
    }

    if (packet.op === 9) {
      this.scheduleReconnect(context, this.policy.invalidSession(packet.d === true));
      return;
    }

    if (packet.t === "READY") {
      this.clearHandshakeDeadline(context);
      const username = packet.d?.user?.username ?? "unknown";
      const discriminator = packet.d?.user?.discriminator ?? "0";
      this.logger.log(`Gateway READY as ${username}#${discriminator}; sequence=${this.policy.sequence}.`);
    } else if (packet.t === "RESUMED") {
      this.clearHandshakeDeadline(context);
      this.logger.log(`Gateway RESUMED; sequence=${this.policy.sequence}.`);
    }

    if (packet.op === 0) {
      try {
        await this.onDispatch(packet);
      } catch (error) {
        this.logger.error(`Gateway dispatch ${packet.t ?? "unknown"} failed: ${errorMessage(error)}.`);
      }
    }
  }

  handleClose(context, event) {
    if (!this.isCurrent(context) || context.reconnectScheduled) return;

    this.clearHeartbeat(context);
    this.clearHandshakeDeadline(context);
    const code = Number.isInteger(event.code) ? event.code : 1006;
    const reason = event.reason ? ` reason=${event.reason}` : "";
    this.logger.log(
      `Gateway closed: code=${code}${reason} sequence=${stateValue(this.policy.sequence)} ` +
      `session=${this.policy.canResume ? "resumable" : "fresh"} heartbeat_ack=${this.policy.heartbeatAcked}.`,
    );

    const decision = this.policy.closed(code);
    if (decision.action === "exit") {
      context.reconnectScheduled = true;
      this.stopped = true;
      this.logger.error(`Gateway close ${code} is terminal; stopping watcher.`);
      this.onFatal(decision);
      return;
    }

    this.scheduleReconnect(context, decision);
  }

  handleError(context, event) {
    if (!this.isCurrent(context) || context.reconnectScheduled) return;
    this.logger.error(
      `Gateway transport error: ${errorMessage(event)}; sequence=${stateValue(this.policy.sequence)} ` +
      `session=${this.policy.canResume ? "resumable" : "fresh"} heartbeat_ack=${this.policy.heartbeatAcked}.`,
    );
    this.scheduleReconnect(context, this.policy.transportFailure("transport_error"));
  }

  scheduleHeartbeat(context, delayMs) {
    this.clearHeartbeat(context);
    context.heartbeatTimer = this.setTimeoutFn(() => {
      context.heartbeatTimer = null;
      if (!this.isCurrent(context) || context.reconnectScheduled) return;

      const decision = this.policy.heartbeatDue();
      if (decision.action === "reconnect") {
        this.logger.error(
          `Gateway heartbeat ACK missing; reconnecting sequence=${stateValue(this.policy.sequence)} ` +
          `session=${this.policy.canResume ? "resumable" : "fresh"}.`,
        );
        this.scheduleReconnect(context, decision);
        return;
      }

      if (this.send(context, decision.payload)) {
        this.scheduleHeartbeat(context, context.heartbeatInterval);
      }
    }, delayMs);
  }

  clearHeartbeat(context) {
    if (context.heartbeatTimer == null) return;
    this.clearTimeoutFn(context.heartbeatTimer);
    context.heartbeatTimer = null;
  }

  scheduleHandshakeDeadline(context, delayMs, reason) {
    this.clearHandshakeDeadline(context);
    context.handshakeTimer = this.setTimeoutFn(() => {
      context.handshakeTimer = null;
      if (!this.isCurrent(context)) return;
      this.logger.error(`Gateway ${reason.replace("_", " ")}; reconnecting.`);
      this.scheduleReconnect(context, this.policy.transportFailure(reason));
    }, delayMs);
  }

  clearHandshakeDeadline(context) {
    if (context.handshakeTimer == null) return;
    this.clearTimeoutFn(context.handshakeTimer);
    context.handshakeTimer = null;
  }

  send(context, packet) {
    if (!this.isCurrent(context) || context.reconnectScheduled) return false;
    try {
      context.socket.send(JSON.stringify(packet));
      return true;
    } catch (error) {
      this.logger.error(`Gateway send failed: ${errorMessage(error)}.`);
      this.scheduleReconnect(context, this.policy.transportFailure("send_error"));
      return false;
    }
  }

  scheduleReconnect(context, decision) {
    if (!this.isCurrent(context) || context.reconnectScheduled || this.stopped) return;
    context.reconnectScheduled = true;
    this.clearHeartbeat(context);
    this.clearHandshakeDeadline(context);
    this.logger.log(
      `Gateway reconnect: reason=${decision.reason} mode=${decision.mode} delay=${decision.delayMs}ms ` +
      `sequence=${stateValue(this.policy.sequence)} heartbeat_ack=${this.policy.heartbeatAcked}.`,
    );

    try {
      context.socket.close(4000, "reconnect");
    } catch {
      // The transport is already gone.
    }
    this.scheduleConnect(decision);
  }

  scheduleConnect(decision) {
    if (this.stopped) return;
    if (this.retryTimer != null) this.clearTimeoutFn(this.retryTimer);
    this.retryTimer = this.setTimeoutFn(() => this.connect(), decision.delayMs);
  }

  isCurrent(context) {
    return !this.stopped && !context.reconnectScheduled && context === this.context;
  }
}

export async function fetchGatewayWithRetry(fetchGateway, {
  sleepFn = (delayMs) => new Promise((resolve) => setTimeout(resolve, delayMs)),
  logger = console,
  retryBaseMs = 500,
  retryMaxMs = 15_000,
} = {}) {
  let attempt = 0;
  while (true) {
    try {
      const gateway = await fetchGateway();
      if (!gateway?.url) {
        const error = new Error("Discord gateway response did not include a URL.");
        error.terminal = true;
        throw error;
      }
      return gateway;
    } catch (error) {
      if (error?.terminal || isTerminalHttpStatus(error?.status)) {
        error.terminal = true;
        error.exitCode = 0;
        throw error;
      }

      const delayMs = attempt === 0
        ? 0
        : Math.min(retryBaseMs * (2 ** (attempt - 1)), retryMaxMs);
      attempt += 1;
      logger.error(
        `Gateway discovery failed: ${errorMessage(error)}; retrying in ${delayMs}ms (attempt ${attempt}).`,
      );
      await sleepFn(delayMs);
    }
  }
}

function errorMessage(value) {
  return String(value?.message ?? value?.error?.message ?? value?.type ?? "unknown");
}

function stateValue(value) {
  return value == null ? "none" : String(value);
}

function isTerminalHttpStatus(value) {
  const status = Number(value);
  return status >= 400 && status < 500 && status !== 408 && status !== 429;
}
