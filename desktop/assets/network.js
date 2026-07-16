"use strict";

(function exposeNetwork(root, factory) {
  const network = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = network;
  } else {
    root.BTC09Network = network;
  }
})(typeof globalThis === "object" ? globalThis : this, function createNetwork() {
  const unreachableMessage = "BTC09 Wallet lost contact with the app. Make sure the app is still running, then try again.";
  const unreadableMessage = "BTC09 Wallet received an unreadable response. Try again.";

  function publicError(code, message) {
    const error = new Error(message);
    error.code = code;
    return error;
  }

  function defaultWait(milliseconds) {
    return new Promise((resolve) => setTimeout(resolve, milliseconds));
  }

  async function request(path, options = {}, hooks = {}) {
    const init = { credentials: "same-origin", ...options };
    const method = String(init.method || "GET").toUpperCase();
    if (method === "POST") {
      init.headers = {
        "Content-Type": "application/json",
        "X-BTC09-CSRF": typeof hooks.csrf === "function" ? hooks.csrf() : "",
        ...(init.headers || {}),
      };
    }

    const fetchRequest = hooks.fetch || globalThis.fetch.bind(globalThis);
    const wait = hooks.wait || defaultWait;
    const attempts = method === "GET" ? 2 : 1;
    let response;
    for (let attempt = 0; attempt < attempts; attempt += 1) {
      try {
        response = await fetchRequest(path, init);
        break;
      } catch {
        if (attempt + 1 < attempts) {
          await wait(250);
          continue;
        }
        throw publicError("wallet_unreachable", unreachableMessage);
      }
    }

    let payload;
    try {
      payload = await response.json();
    } catch {
      throw publicError("invalid_response", unreadableMessage);
    }
    if (!response.ok || !payload?.ok) {
      throw publicError(
        payload?.error?.code || "request_failed",
        payload?.error?.message || "BTC09 Wallet could not complete that action.",
      );
    }
    return payload.data;
  }

  return { request };
});
