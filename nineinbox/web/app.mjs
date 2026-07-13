import {
  MAX_CONTENT_BYTES,
  createItemId,
  createPairingBundle,
  decodePairingFragment,
  decryptItem,
  encodePairingFragment,
  encryptItem,
  exportRecoveryFile,
  hashToken,
  safetyWords,
} from "./crypto.mjs";
import { renderPairingQR } from "./qr.mjs";
import { createInboxStorage } from "./storage.mjs";

const API_PATH = "/api/nine/v1/inboxes";
const storage = createInboxStorage();
const state = { bundle: null, settings: null, items: [], syncTimer: null, pendingFile: null, objectURLs: new Set() };
const byId = (id) => document.getElementById(id);

function encodeToken(value) {
  let binary = "";
  for (const byte of value) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

function showToast(message, error = false) {
  const toast = byId("toast");
  toast.textContent = message;
  toast.classList.toggle("is-error", error);
  toast.hidden = false;
  clearTimeout(showToast.timer);
  showToast.timer = setTimeout(() => { toast.hidden = true; }, 4600);
}

function setBusy(button, busy, label) {
  if (!button.dataset.label) button.dataset.label = button.textContent.trim();
  button.disabled = busy;
  button.textContent = busy ? label : button.dataset.label;
}

function showView(id) {
  for (const view of ["setup-view", "pair-view", "pair-device-view", "inbox-view"]) byId(view).hidden = view !== id;
  window.scrollTo({ top: 0, behavior: "smooth" });
}

function updateConnection() {
  const online = navigator.onLine;
  document.querySelector(".connection").classList.toggle("is-online", online);
  document.querySelector(".connection").classList.toggle("is-offline", !online);
  byId("connection-label").textContent = online ? "Ready" : "Offline";
  if (state.bundle) byId("sync-note").textContent = online ? "Syncs while this page is open." : "Offline. Your saved items are still available on this device.";
}

async function api(path, options = {}) {
  const response = await fetch((state.bundle?.apiBase || location.origin) + path, {
    cache: "no-store",
    ...options,
  });
  if (!response.ok) {
    let code = "request_failed";
    try { code = (await response.json()).error?.code || code; } catch { /* fixed fallback */ }
    const messages = {
      unauthorized: "This device no longer has access to that inbox.",
      not_found: "That inbox item is no longer available.",
      expired: "That item has expired.",
      too_large: "That item is larger than the service allows.",
      inbox_full: "This inbox is full. Delete something and try again.",
      service_full: "Nine Inbox storage is temporarily full. Try again later.",
      rate_limited: "Too many requests. Wait a moment and try again.",
    };
    throw new Error(messages[code] || `Nine Inbox could not complete that request (${response.status}).`);
  }
  return response;
}

async function createInbox() {
  const button = byId("create-inbox");
  setBusy(button, true, "Creating…");
  try {
    const bundle = createPairingBundle(location.origin);
    const [writeHash, recoveryHash] = await Promise.all([hashToken(bundle.writeToken), hashToken(bundle.recoveryToken)]);
    const response = await api(API_PATH, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ write_token_hash: encodeToken(writeHash), recovery_token_hash: encodeToken(recoveryHash) }),
    });
    const result = await response.json();
    bundle.mailboxId = result.data.id;
    await storage.saveBundle(bundle);
    state.bundle = bundle;
    await showPairing();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function pairingValue(input) {
  const value = input.trim();
  const marker = "#pair=";
  if (value.includes(marker)) return value.slice(value.indexOf(marker) + marker.length).split("&", 1)[0];
  return value.replace(/^#?pair=/u, "");
}

async function pairDevice(event) {
  event.preventDefault();
  const button = event.submitter;
  setBusy(button, true, "Pairing…");
  try {
    const bundle = decodePairingFragment(pairingValue(byId("pair-code").value));
    if (bundle.apiBase !== location.origin) throw new Error("Open the pairing link on the same Nine Inbox site that created it.");
    await storage.saveBundle(bundle);
    state.bundle = bundle;
    history.replaceState(null, "", "/inbox/");
    await startInbox();
    showToast("Device paired. Compare the safety words with your other screen.");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function showPairing() {
  const fragment = encodePairingFragment(state.bundle);
  const link = `${location.origin}/inbox/#pair=${fragment}`;
  byId("pairing-link").value = link;
  byId("pairing-words").textContent = await safetyWords(state.bundle, "nine-inbox-fingerprint", "nine-inbox-fingerprint");
  renderPairingQR(byId("pairing-qr"), link);
  showView("pair-view");
}

async function copyText(value, message = "Copied.") {
  try {
    await navigator.clipboard.writeText(value);
    showToast(message);
  } catch {
    showToast("Copy was blocked. Select the text and copy it manually.", true);
  }
}

function authHeaders(extra = {}) {
  return { Authorization: `Bearer ${encodeToken(state.bundle.writeToken)}`, ...extra };
}

function formatBytes(value) {
  if (value >= 1024 * 1024) return `${(value / (1024 * 1024)).toFixed(value >= 10 * 1024 * 1024 ? 0 : 1)} MiB`;
  if (value >= 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${value} B`;
}

function clearObjectURLs() {
  for (const url of state.objectURLs) URL.revokeObjectURL(url);
  state.objectURLs.clear();
}

function itemTitle(item) {
  if (item.kind === "text") return item.text.split(/\r?\n/u, 1)[0].slice(0, 100);
  if (item.kind === "link") return item.text;
  return item.name;
}

function actionButton(label, action, id) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  button.dataset.action = action;
  button.dataset.id = id;
  return button;
}

function renderItems() {
  clearObjectURLs();
  const query = byId("inbox-search").value.trim().toLocaleLowerCase();
  const visible = state.items.filter((item) => !query || [item.text, item.name, item.type, item.device].some((value) => String(value || "").toLocaleLowerCase().includes(query)));
  const list = byId("inbox-list");
  list.replaceChildren();
  for (const item of visible) {
    const row = document.createElement("article");
    row.className = "inbox-item";
    const side = document.createElement("div");
    const kind = document.createElement("span");
    kind.className = "item-kind";
    kind.textContent = item.kind;
    const time = document.createElement("time");
    time.className = "item-time";
    time.dateTime = item.createdAt;
    time.textContent = new Date(item.createdAt).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
    side.append(kind, time);
    const main = document.createElement("div");
    main.className = "item-main";
    const title = document.createElement("strong");
    title.textContent = itemTitle(item) || "Untitled item";
    main.append(title);
    if (item.kind === "text") {
      const text = document.createElement("p");
      text.textContent = item.text;
      main.append(text);
    }
    const meta = document.createElement("div");
    meta.className = "item-meta";
    meta.textContent = `${item.device} · ${formatBytes(item.size)} · expires ${new Date(item.expiresAt).toLocaleDateString()}`;
    main.append(meta);
    const actions = document.createElement("div");
    actions.className = "item-actions";
    if (item.kind === "text" || item.kind === "link") actions.append(actionButton("Copy", "copy", item.id));
    if (item.kind === "link") {
      const open = document.createElement("a");
      open.textContent = "Open";
      open.href = item.text;
      open.target = "_blank";
      open.rel = "noopener noreferrer";
      actions.append(open);
    }
    if (item.kind === "file" || item.kind === "photo") {
      const blob = new Blob([item.data], { type: item.type || "application/octet-stream" });
      const url = URL.createObjectURL(blob);
      state.objectURLs.add(url);
      const download = document.createElement("a");
      download.textContent = item.kind === "photo" ? "Preview" : "Download";
      download.href = url;
      download.download = item.name;
      download.target = "_blank";
      actions.append(download);
    }
    const remove = actionButton("Delete", "delete", item.id);
    remove.classList.add("delete");
    actions.append(remove);
    row.append(side, main, actions);
    list.append(row);
  }
  byId("empty-inbox").hidden = state.items.length !== 0 || query !== "";
  const used = state.items.reduce((total, item) => total + Number(item.ciphertextSize || 0), 0);
  byId("storage-meter").value = used;
  byId("storage-copy").textContent = `${formatBytes(used)} of 50 MiB`;
}

async function loadCachedItems() {
  state.items = await storage.listCachedItems();
  renderItems();
}

async function syncInbox({ quiet = false } = {}) {
  if (!state.bundle || !navigator.onLine) return;
  if (!quiet) byId("sync-note").textContent = "Checking for new items…";
  try {
    const response = await api(`${API_PATH}/${state.bundle.mailboxId}`, { headers: authHeaders() });
    const remote = (await response.json()).data.items || [];
    const remoteIDs = new Set(remote.map((item) => item.id));
    for (const header of remote) {
      if (await storage.getCachedItem(header.id)) continue;
      const fetched = await api(`${API_PATH}/${state.bundle.mailboxId}/items/${header.id}`, { headers: authHeaders() });
      const ciphertext = new Uint8Array(await fetched.arrayBuffer());
      const item = await decryptItem(ciphertext, state.bundle, state.bundle.mailboxId, header.id);
      await storage.putCachedItem({ ...item, ...header, ciphertextSize: ciphertext.length });
    }
    for (const item of await storage.listCachedItems()) {
      if (!remoteIDs.has(item.id)) await storage.deleteCachedItem(item.id);
    }
    await loadCachedItems();
    byId("sync-note").textContent = `Up to date · ${new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`;
  } catch (error) {
    byId("sync-note").textContent = "Could not sync. Saved items remain on this device.";
    if (!quiet) showToast(error.message, true);
  }
}

async function sendItem(event) {
  event.preventDefault();
  const button = byId("send-item");
  const text = byId("item-text").value.trim();
  const file = state.pendingFile || byId("item-file").files[0] || null;
  if ((text && file) || (!text && !file)) {
    showToast("Send either a note/link or one file.", true);
    return;
  }
  if (file && file.size > MAX_CONTENT_BYTES) {
    showToast("Files must be 20 MiB or smaller.", true);
    return;
  }
  setBusy(button, true, "Encrypting…");
  try {
    const now = new Date();
    const looksLikeLink = text && /^https?:\/\/\S+$/iu.test(text);
    const pinned = byId("pin-item").checked && !file;
    const expires = new Date(now.getTime() + (pinned ? 30 : 7) * 86400000);
    const data = file ? new Uint8Array(await file.arrayBuffer()) : new Uint8Array();
    const kind = file ? (file.type.startsWith("image/") ? "photo" : "file") : (looksLikeLink ? "link" : "text");
    const id = createItemId();
    const input = {
      kind,
      name: file?.name || "",
      type: file?.type || (looksLikeLink ? "text/uri-list" : "text/plain"),
      device: state.settings.deviceLabel,
      createdAt: now.toISOString(),
      expiresAt: expires.toISOString(),
      text: file ? "" : text,
      data,
    };
    const ciphertext = await encryptItem(input, state.bundle, state.bundle.mailboxId, id);
    setBusy(button, true, "Sending…");
    const response = await api(`${API_PATH}/${state.bundle.mailboxId}/items`, {
      method: "POST",
      headers: authHeaders({
        "Content-Type": "application/octet-stream",
        "X-Nine-Retention": pinned ? "pinned" : "standard",
        "X-Nine-Item-ID": id,
      }),
      body: ciphertext,
    });
    const header = (await response.json()).data;
    await storage.putCachedItem({ ...input, ...header, ciphertextSize: ciphertext.length });
    byId("composer-sheet").close();
    resetComposer();
    await loadCachedItems();
    showToast("Sent to your inbox.");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function resetComposer() {
  byId("composer").reset();
  byId("chosen-file").textContent = "No file selected";
  state.pendingFile = null;
}

async function handleItemAction(event) {
  const control = event.target.closest("[data-action]");
  if (!control || !control.dataset.id) return;
  const item = state.items.find((candidate) => candidate.id === control.dataset.id);
  if (!item) return;
  if (control.dataset.action === "copy") {
    await copyText(item.text, "Item copied.");
  } else if (control.dataset.action === "delete") {
    if (!confirm("Delete this item from every paired device?")) return;
    try {
      await api(`${API_PATH}/${state.bundle.mailboxId}/items/${item.id}`, { method: "DELETE", headers: authHeaders() });
      await storage.deleteCachedItem(item.id);
      await loadCachedItems();
      showToast("Item deleted.");
    } catch (error) { showToast(error.message, true); }
  }
}

async function exportRecovery(event) {
  event.preventDefault();
  const password = prompt("Choose a recovery password (at least 12 characters). It cannot be reset.");
  if (password == null) return;
  const confirmPassword = prompt("Enter the recovery password again.");
  if (password !== confirmPassword) { showToast("Passwords did not match.", true); return; }
  try {
    const content = await exportRecoveryFile(state.bundle, password);
    const url = URL.createObjectURL(new Blob([content], { type: "application/json" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `nine-inbox-recovery-${state.bundle.mailboxId.slice(0, 8)}.nine`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    showToast("Encrypted recovery file saved.");
  } catch (error) { showToast(error.message, true); }
}

async function startInbox() {
  state.settings = await storage.loadSettings();
  byId("device-label").value = state.settings.deviceLabel;
  showView("inbox-view");
  await loadCachedItems();
  await syncInbox({ quiet: true });
  clearTimeout(state.syncTimer);
  state.syncTimer = setTimeout(scheduleSync, 15000);
}

async function scheduleSync() {
  try {
    await syncInbox({ quiet: true });
  } finally {
    clearTimeout(state.syncTimer);
    state.syncTimer = setTimeout(scheduleSync, 15000);
  }
}

async function loadShareTarget() {
  if (!location.search.includes("share=1") || !globalThis.caches) return;
  try {
    const cache = await caches.open("nine-inbox-shares-v1");
    const response = await cache.match("/inbox/__pending-share");
    if (!response) return;
    await cache.delete("/inbox/__pending-share");
    const kind = response.headers.get("X-Nine-Share-Kind");
    const name = decodeURIComponent(response.headers.get("X-Nine-Share-Name") || "shared-file");
    if (kind === "file") state.pendingFile = new File([await response.blob()], name, { type: response.headers.get("Content-Type") || "application/octet-stream" });
    else byId("item-text").value = await response.text();
    byId("chosen-file").textContent = state.pendingFile ? `${state.pendingFile.name} · ${formatBytes(state.pendingFile.size)}` : "No file selected";
    byId("composer-sheet").showModal();
    history.replaceState(null, "", "/inbox/");
  } catch { showToast("The shared item could not be opened.", true); }
}

function bindEvents() {
  byId("create-inbox").addEventListener("click", createInbox);
  byId("open-pair").addEventListener("click", () => showView("pair-device-view"));
  document.querySelectorAll('[data-action="back"]').forEach((button) => button.addEventListener("click", () => showView(state.bundle ? "inbox-view" : "setup-view")));
  byId("pair-device-form").addEventListener("submit", pairDevice);
  byId("copy-pairing").addEventListener("click", () => copyText(byId("pairing-link").value, "Pairing link copied."));
  byId("finish-pairing").addEventListener("click", startInbox);
  byId("show-pairing").addEventListener("click", showPairing);
  byId("open-settings").addEventListener("click", () => byId("settings-sheet").showModal());
  byId("open-composer").addEventListener("click", () => byId("composer-sheet").showModal());
  byId("composer").addEventListener("submit", sendItem);
  byId("item-file").addEventListener("change", (event) => {
    state.pendingFile = null;
    const file = event.target.files[0];
    byId("chosen-file").textContent = file ? `${file.name} · ${formatBytes(file.size)}` : "No file selected";
  });
  byId("inbox-search").addEventListener("input", renderItems);
  byId("inbox-list").addEventListener("click", handleItemAction);
  byId("save-device").addEventListener("click", async (event) => {
    event.preventDefault();
    try {
      state.settings = { ...state.settings, deviceLabel: byId("device-label").value.trim() };
      await storage.saveSettings(state.settings);
      byId("settings-sheet").close();
      showToast("Device name saved.");
    } catch (error) { showToast(error.message, true); }
  });
  byId("recovery-export").addEventListener("click", exportRecovery);
  byId("forget-device").addEventListener("click", async (event) => {
    event.preventDefault();
    if (!confirm("Forget this inbox on this device? Export recovery first if this is your last paired device.")) return;
    await storage.clearBundle();
    await storage.clearCachedItems();
    location.replace("/inbox/");
  });
  window.addEventListener("online", () => { updateConnection(); syncInbox({ quiet: true }); });
  window.addEventListener("offline", updateConnection);
  document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") syncInbox({ quiet: true }); });
}

async function boot() {
  bindEvents();
  updateConnection();
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/inbox/service-worker.js", { scope: "/inbox/" }).catch(() => {});
  state.bundle = await storage.loadBundle();
  const fragment = location.hash.startsWith("#pair=") ? location.hash : "";
  if (fragment && !state.bundle) {
    byId("pair-code").value = location.href;
    showView("pair-device-view");
    return;
  }
  if (state.bundle) {
    await startInbox();
    await loadShareTarget();
  } else {
    showView("setup-view");
  }
}

boot().catch((error) => {
  showView("setup-view");
  showToast(error.message || "Nine Inbox could not start.", true);
});
