const invoke = window.__TAURI__?.core?.invoke;
const requestedDemo = new URLSearchParams(window.location.search).get("demo");
const demoScreen = navigator.webdriver ? requestedDemo : null;
const pluginPrefix = "plugin:wallet-core|";

const state = {
  status: null,
  activity: [],
  pending: null,
  recoveryPhrase: null,
  screen: null,
  locked: false,
  toastTimer: null,
};

const byId = (id) => document.getElementById(id);
const screens = [...document.querySelectorAll("[data-screen]")];
const mainTabs = new Set(["home", "activity", "settings"]);
const routeNames = new Set(screens.map((screen) => screen.dataset.screen));

async function call(command, payload) {
  if (demoScreen) return demoCall(command, payload);
  if (!invoke) throw new Error("BTC09 Wallet could not start safely.");
  const raw = await invoke(`${pluginPrefix}${command}`, payload ? { payload } : {});
  if (command === "recovery_phrase") return raw;
  return raw ? JSON.parse(raw) : {};
}

function showScreen(name, options = {}) {
  state.screen = name;
  for (const screen of screens) screen.hidden = screen.dataset.screen !== name;
  const showChrome = !["onboarding", "unlock", "backup"].includes(name);
  byId("app-bar").hidden = !showChrome;
  byId("mobile-nav").hidden = !mainTabs.has(name);
  for (const button of document.querySelectorAll("[data-nav]")) {
    const selected = button.dataset.nav === name;
    button.classList.toggle("active", selected);
    if (selected) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  if (!options.keepScroll) window.scrollTo({ top: 0, behavior: "instant" });
  byId("app-main").focus({ preventScroll: true });
}

function showRoute(name, mode = "push") {
  const current = history.state?.btc09Wallet ? history.state : null;
  const currentDepth = Number(current?.depth || 0);
  const route = { btc09Wallet: true, screen: name, depth: mode === "reset" ? 0 : currentDepth };
  if (mode === "push" && current?.screen !== name) {
    route.depth = currentDepth + 1;
    history.pushState(route, "", `#${name}`);
  } else {
    history.replaceState(route, "", `#${name}`);
  }
  showScreen(name);
}

function goBack(fallback = "home") {
  const depth = Number(history.state?.btc09Wallet ? history.state.depth : 0);
  if (depth > 0) history.back();
  else showRoute(fallback, "replace");
}

async function handleHistory(event) {
  let target = event.state?.btc09Wallet && routeNames.has(event.state.screen)
    ? event.state.screen
    : "home";
  if (state.locked) target = "unlock";
  if (state.status?.wallet_state === "missing") target = "onboarding";
  if (state.screen === "review" && target !== "review") await cancelPending();
  if (state.screen === "backup" && target !== "backup") clearRecoveryWords();
  if (target !== event.state?.screen) {
    showRoute(target, "replace");
    return;
  }
  showScreen(target);
}

function setBusy(active, label = "Working…") {
  byId("busy-label").textContent = label;
  byId("busy-cover").hidden = !active;
}

function toast(message, error = false) {
  const node = byId("toast");
  clearTimeout(state.toastTimer);
  node.textContent = message;
  node.classList.toggle("error", error);
  node.hidden = false;
  state.toastTimer = setTimeout(() => { node.hidden = true; }, 3200);
}

function publicError(error) {
  const message = String(error?.message || error || "").trim();
  const allowed = /^(The |A |Check |Enter |Choose |Finish |Unlock |Set up |BTC09 Wallet)/;
  if (allowed.test(message) && message.length <= 180) return message;
  return "BTC09 Wallet couldn't complete that. Try again.";
}

async function run(label, operation) {
  setBusy(true, label);
  try {
    return await operation();
  } catch (error) {
    toast(publicError(error), true);
    return null;
  } finally {
    setBusy(false);
  }
}

function formatUnits(value, minimumDigits = 0) {
  const units = BigInt(value ?? 0);
  const negative = units < 0n;
  const absolute = negative ? -units : units;
  const whole = absolute / 100000000n;
  const fraction = (absolute % 100000000n).toString().padStart(8, "0").replace(/0+$/, "");
  const padded = fraction.padEnd(minimumDigits, "0");
  return `${negative ? "−" : ""}${whole.toLocaleString("en-US")}${padded ? `.${padded}` : ""}`;
}

function shorten(value, start = 8, end = 6) {
  if (!value || value.length <= start + end + 1) return value || "—";
  return `${value.slice(0, start)}…${value.slice(-end)}`;
}

function normalizePhrase(value) {
  return value.trim().toLowerCase().split(/\s+/).filter(Boolean).join(" ");
}

function setNetwork(syncState) {
  const pill = byId("network-pill");
  pill.className = "network-pill";
  if (syncState === "connected") {
    pill.classList.add("connected");
    pill.querySelector("span").textContent = "Connected";
  } else if (syncState === "unavailable") {
    pill.classList.add("unavailable");
    pill.querySelector("span").textContent = "Network busy";
  } else {
    pill.querySelector("span").textContent = "Wallet ready";
  }
}

function renderStatus(status) {
  state.status = status;
  setNetwork(status.sync_state);
  const balance = status.balance_available ? formatUnits(status.balance_units) : "—";
  byId("balance-value").textContent = balance;
  byId("send-available").textContent = status.balance_available ? `${balance} 09C` : "Unavailable";
  byId("home-send").disabled = !status.send_available;
  if (status.sync_state === "connected") {
    const immature = BigInt(status.immature_units || 0);
    byId("balance-note").textContent = immature > 0n
      ? `${formatUnits(immature)} 09C waiting for confirmations`
      : `Up to date · block ${Number(status.height || 0).toLocaleString("en-US")}`;
  } else {
    byId("balance-note").textContent = "The network is unavailable. Your wallet and keys are safe.";
  }
}

function activityName(item) {
  if (item.kind === "received" || BigInt(item.net_units || 0) > 0n) return "Received";
  if (item.kind === "sent" || BigInt(item.net_units || 0) < 0n) return "Sent";
  return "Transaction";
}

function activityStatus(item) {
  if (item.status === "immature") return `${item.blocks_until_mature || 0} blocks until available`;
  if (item.status === "pending" || Number(item.confirmations || 0) === 0) return "Pending";
  const count = Number(item.confirmations || 0);
  return `${count.toLocaleString("en-US")} confirmation${count === 1 ? "" : "s"}`;
}

function activityNode(item) {
  const amount = BigInt(item.net_units || 0);
  const direction = amount > 0n ? "received" : amount < 0n ? "sent" : "neutral";
  const sign = amount > 0n ? "+" : "";
  const glyph = direction === "received" ? "↓" : direction === "sent" ? "↑" : "·";
  const row = document.createElement("div");
  row.className = `activity-item ${direction}`;
  const icon = document.createElement("span");
  icon.className = "activity-glyph";
  icon.setAttribute("aria-hidden", "true");
  icon.textContent = glyph;
  const copy = document.createElement("span");
  copy.className = "activity-copy";
  const title = document.createElement("strong");
  title.textContent = activityName(item);
  const detail = document.createElement("small");
  detail.textContent = `${activityStatus(item)} · ${shorten(item.txid)}`;
  copy.append(title, detail);
  const value = document.createElement("span");
  value.className = `activity-value${amount > 0n ? " positive" : ""}`;
  value.textContent = `${sign}${formatUnits(amount)} 09C`;
  row.append(icon, copy, value);
  return row;
}

function replaceActivity(container, items) {
  if (items.length) {
    container.replaceChildren(...items.map(activityNode));
    return;
  }
  const empty = document.createElement("div");
  empty.className = "empty-state";
  empty.textContent = "No transactions yet. Your first payment will appear here.";
  container.replaceChildren(empty);
}

function renderActivity(items) {
  state.activity = items;
  replaceActivity(byId("home-activity"), items.slice(0, 3));
  replaceActivity(byId("full-activity"), items);
}

async function loadActivity(silent = false) {
  const action = async () => {
    const result = await call("activity", { limit: 100 });
    renderActivity(result.items || []);
    return result;
  };
  if (silent) {
    try { return await action(); } catch { renderActivity([]); return null; }
  }
  return run("Loading activity…", action);
}

async function refreshWallet(options = {}) {
  const status = await (options.busy
    ? run("Checking wallet…", () => call("status"))
    : call("status").catch(() => null));
  if (!status) return;
  if (status.wallet_state === "missing") {
    state.locked = false;
    showRoute("onboarding", "replace");
    return;
  }
  if (status.wallet_state === "locked" || status.needs_unlock) {
    state.locked = true;
    showRoute("unlock", "replace");
    return;
  }
  state.locked = false;
  renderStatus(status);
  await loadActivity(true);
  if (!options.stay || ["onboarding", "unlock"].includes(state.screen)) showRoute("home", "replace");
}

function renderRecoveryWords(phrase, returning = false) {
  state.recoveryPhrase = phrase;
  const words = normalizePhrase(phrase).split(" ");
  byId("recovery-words").replaceChildren(...words.map((word) => {
    const item = document.createElement("li");
    item.textContent = word;
    return item;
  }));
  byId("backup-back").hidden = !returning;
  byId("backup-check-row").hidden = returning;
  byId("finish-backup").hidden = returning;
  byId("backup-confirmed").checked = false;
  byId("finish-backup").disabled = true;
  showRoute("backup", returning ? "push" : "replace");
}

function clearRecoveryWords() {
  state.recoveryPhrase = null;
  byId("recovery-words").replaceChildren();
}

async function openReceive(mode = "push") {
  const result = await run("Preparing your address…", () => call("receive"));
  if (!result) return;
  byId("receive-address").textContent = result.address;
  byId("receive-qr").src = result.qr_data_url || `data:image/png;base64,${result.qr_png_base64}`;
  showRoute("receive", mode);
}

function renderReview(preview, mode = "push") {
  state.pending = preview;
  byId("review-amount").textContent = `${formatUnits(preview.amount_units)} 09C`;
  byId("review-address").textContent = preview.destination;
  byId("review-fee").textContent = `${formatUnits(preview.fee_units)} 09C`;
  byId("review-total").textContent = `${formatUnits(preview.total_units)} 09C`;
  byId("review-code").textContent = preview.confirmation_code;
  showRoute("review", mode);
}

async function cancelPending() {
  if (state.pending?.pending_id) {
    try { await call("cancel_send", { pendingId: state.pending.pending_id }); } catch { /* The preview expires safely. */ }
  }
  state.pending = null;
}

async function navigate(name) {
  if (name !== "review" && state.screen === "review") await cancelPending();
  if (name === "receive") return openReceive();
  if (name === "activity") await loadActivity(false);
  showRoute(name);
}

function bindEvents() {
  for (const button of document.querySelectorAll("[data-go], [data-nav]")) {
    button.addEventListener("click", () => navigate(button.dataset.go || button.dataset.nav));
  }
  for (const button of document.querySelectorAll("[data-back]")) {
    button.addEventListener("click", () => goBack(button.dataset.back));
  }

  byId("show-restore").addEventListener("click", () => {
    byId("restore-form").hidden = false;
    byId("show-restore").hidden = true;
    byId("restore-words").focus();
  });
  byId("hide-restore").addEventListener("click", () => {
    byId("restore-form").hidden = true;
    byId("show-restore").hidden = false;
    byId("restore-words").value = "";
    byId("restore-count").textContent = "0 of 24 words";
  });
  byId("restore-words").addEventListener("input", (event) => {
    const count = normalizePhrase(event.target.value).split(" ").filter(Boolean).length;
    byId("restore-count").textContent = `${count} of 24 words`;
  });

  byId("create-wallet").addEventListener("click", async () => {
    const result = await run("Creating wallet…", () => call("create_wallet"));
    if (result) renderRecoveryWords(result.recovery_phrase);
  });
  byId("restore-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const phrase = normalizePhrase(byId("restore-words").value);
    if (phrase.split(" ").length !== 24) return toast("Enter all 24 recovery words.", true);
    const result = await run("Restoring wallet…", () => call("restore_wallet", { recoveryPhrase: phrase }));
    if (result) {
      byId("restore-words").value = "";
      await refreshWallet();
    }
  });
  byId("unlock-wallet").addEventListener("click", async () => {
    const result = await run("Opening wallet…", () => call("unlock"));
    if (result) await refreshWallet();
  });

  byId("backup-confirmed").addEventListener("change", (event) => { byId("finish-backup").disabled = !event.target.checked; });
  byId("finish-backup").addEventListener("click", async () => {
    clearRecoveryWords();
    await refreshWallet();
  });
  byId("backup-back").addEventListener("click", () => { clearRecoveryWords(); goBack("settings"); });

  byId("refresh-wallet").addEventListener("click", async () => { await refreshWallet({ busy: true, stay: true }); });
  byId("refresh-activity").addEventListener("click", async () => { await loadActivity(false); });

  byId("copy-address").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(byId("receive-address").textContent); toast("Address copied"); }
    catch { toast("Press and hold the address to copy it.", true); }
  });
  byId("share-address").addEventListener("click", async () => {
    const address = byId("receive-address").textContent;
    if (navigator.share) {
      try { await navigator.share({ title: "My BTC09 address", text: address }); } catch { /* The user cancelled. */ }
    } else {
      try { await navigator.clipboard.writeText(address); toast("Address copied"); }
      catch { toast("Press and hold the address to copy it.", true); }
    }
  });
  byId("paste-address").addEventListener("click", async () => {
    try { byId("send-address").value = (await navigator.clipboard.readText()).trim(); }
    catch { toast("Press and hold the address field to paste.", true); }
  });

  byId("send-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const destination = byId("send-address").value.trim();
    const amount = byId("send-amount").value.trim();
    const fee = byId("send-fee").value.trim();
    const result = await run("Preparing payment…", () => call("preview_send", { destination, amount, fee }));
    if (result) renderReview(result);
  });
  byId("cancel-review").addEventListener("click", async () => { await cancelPending(); goBack("send"); });
  byId("confirm-send").addEventListener("click", async () => {
    if (!state.pending?.pending_id) return toast("Review the payment again.", true);
    const result = await run("Sending payment…", () => call("confirm_send", { pendingId: state.pending.pending_id }));
    if (!result) return;
    state.pending = null;
    byId("send-form").reset();
    byId("send-fee").value = "0.0001";
    toast("Payment sent");
    await refreshWallet();
  });

  byId("show-recovery").addEventListener("click", async () => {
    const phrase = await run("Checking device security…", () => call("recovery_phrase"));
    if (phrase) renderRecoveryWords(phrase, true);
  });
  byId("lock-wallet").addEventListener("click", async () => {
    await cancelPending();
    await call("lock").catch(() => null);
    state.locked = true;
    showRoute("unlock", "replace");
  });

  window.addEventListener("popstate", handleHistory);
  document.addEventListener("visibilitychange", async () => {
    if (document.visibilityState !== "hidden" || demoScreen) return;
    await cancelPending();
    clearRecoveryWords();
    await call("lock").catch(() => null);
    state.locked = true;
    showRoute("unlock", "replace");
  });
}

function mockQR() {
  const cells = Array.from({ length: 17 }, (_, row) => Array.from({ length: 17 }, (_, column) => {
    const finder = (row < 5 && column < 5) || (row < 5 && column > 11) || (row > 11 && column < 5);
    return finder || ((row * 7 + column * 11 + row * column) % 5 < 2)
      ? `<rect x="${column}" y="${row}" width="1" height="1"/>` : "";
  }).join("")).join("");
  return btoa(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="-2 -2 21 21"><rect x="-2" y="-2" width="21" height="21" fill="white"/><g fill="#171713">${cells}</g></svg>`);
}

const demoAddress = "4d8kwx6xn65W5LwJV4qRJubSkuyHg124kn";
const demoPhrase = "anchor begin canvas daring elder fabric globe harbor ivory jungle kitten lunar maple noble olive picnic quiet river solar timber uncover velvet willow youth";
const demoStatus = {
  wallet_state: "ready", sync_state: "connected", balance_available: true, send_available: true,
  balance_units: 12845000000, immature_units: 0, height: 8841, address: demoAddress,
};
const demoItems = [
  { txid: "5f0c5bf4201a76f3d8a3b0e870424adb90cc6d03969948128ce233be67891f08", kind: "received", status: "confirmed", net_units: 2500000000, confirmations: 38 },
  { txid: "0a63171dd9b329012ba3aa31e6b8ce56cd799a16858aa53fd3bab8e46fd603f1", kind: "sent", status: "confirmed", net_units: -420000000, confirmations: 112 },
  { txid: "d4c7c9a6c2f164849df52db7235c05238baa828a744b97a1c8696139122cfbad", kind: "received", status: "immature", net_units: 5000000000, confirmations: 9, blocks_until_mature: 91 },
];

function demoCall(command, payload) {
  if (command === "status") return Promise.resolve(demoStatus);
  if (command === "activity") return Promise.resolve({ items: demoItems, height: demoStatus.height });
  if (command === "receive") return Promise.resolve({ address: demoAddress, qr_data_url: `data:image/svg+xml;base64,${mockQR()}` });
  if (command === "recovery_phrase") return Promise.resolve(demoPhrase);
  if (command === "create_wallet") return Promise.resolve({ address: demoAddress, recovery_phrase: demoPhrase });
  if (command === "restore_wallet" || command === "unlock") return Promise.resolve(demoStatus);
  if (command === "preview_send") return Promise.resolve({
    pending_id: "preview", destination: payload.destination, amount_units: 125000000,
    fee_units: 10000, total_units: 125010000, confirmation_code: "09A4C2",
  });
  if (command === "confirm_send") return Promise.resolve({ txid: "1".repeat(64), status: "submitted" });
  return Promise.resolve({});
}

async function start() {
  bindEvents();
  if (demoScreen) {
    renderStatus(demoStatus);
    renderActivity(demoItems);
    if (demoScreen === "onboarding") return showRoute("onboarding", "reset");
    if (demoScreen === "locked") { state.locked = true; return showRoute("unlock", "reset"); }
    if (demoScreen === "backup") return renderRecoveryWords(demoPhrase);
    if (demoScreen === "receive") { await openReceive("reset"); return; }
    if (demoScreen === "send") return showRoute("send", "reset");
    if (demoScreen === "review") {
      renderReview({ pending_id: "preview", destination: demoAddress, amount_units: 125000000, fee_units: 10000, total_units: 125010000, confirmation_code: "09A4C2" }, "reset");
      return;
    }
    return showRoute(mainTabs.has(demoScreen) ? demoScreen : "home", "reset");
  }
  await refreshWallet({ busy: true });
}

start();
