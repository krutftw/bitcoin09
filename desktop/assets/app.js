"use strict";

const state = {
  csrf: "",
  status: null,
  pending: null,
  toastTimer: null,
};

const byId = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const init = { credentials: "same-origin", ...options };
  if (init.method === "POST") {
    init.headers = {
      "Content-Type": "application/json",
      "X-BTC09-CSRF": state.csrf,
      ...(init.headers || {}),
    };
  }
  const response = await fetch(path, init);
  const payload = await response.json();
  if (!response.ok || !payload.ok) {
    const error = new Error(payload?.error?.message || "BTC09 Wallet could not complete that action.");
    error.code = payload?.error?.code || "request_failed";
    throw error;
  }
  return payload.data;
}

function formatCoins(units) {
  const value = BigInt(units || 0);
  const whole = value / 100000000n;
  const fraction = (value % 100000000n).toString().padStart(8, "0");
  return `${whole}.${fraction}`;
}

function setBusy(button, busy, label) {
  button.disabled = busy;
  if (!button.dataset.label) button.dataset.label = button.textContent.trim();
  button.textContent = busy ? label : button.dataset.label;
}

function showToast(message, isError = false) {
  const toast = byId("toast");
  clearTimeout(state.toastTimer);
  toast.textContent = message;
  toast.classList.toggle("is-error", isError);
  toast.hidden = false;
  state.toastTimer = setTimeout(() => { toast.hidden = true; }, 5200);
}

function suggestedBackupPath(walletPath) {
  if (!walletPath) return "btc09-wallet-backup.json";
  const separator = walletPath.includes("\\") ? "\\" : "/";
  const parts = walletPath.split(separator);
  parts[parts.length - 1] = "btc09-wallet-backup.json";
  return parts.join(separator);
}

function renderStatus(status) {
  state.status = status;
  state.csrf = status.csrf_token || state.csrf;
  byId("app-version").textContent = status.version || "—";
  byId("network-state").textContent = (status.network || "unknown").replace("btc09-", "").toUpperCase();
  byId("chain-height").textContent = Number(status.height || 0).toLocaleString();
  byId("peer-count").textContent = String(status.peer_count || 0).padStart(2, "0");
  byId("sync-state").textContent = status.sync_state || "offline";
  byId("status-lamp").classList.toggle("is-online", status.peer_count > 0);
  byId("loading-view").hidden = true;
  byId("first-run").hidden = status.wallet_exists;
  byId("wallet-view").hidden = !status.wallet_exists;
  byId("first-run-path").textContent = status.wallet_path || "—";

  if (!status.wallet_exists) return;
  const formatted = formatCoins(status.balance_units).split(".");
  byId("balance-major").textContent = Number(formatted[0]).toLocaleString();
  byId("balance-minor").textContent = `.${formatted[1]}`;
  byId("tip-hash").textContent = status.tip_hash ? `Tip ${status.tip_hash.slice(0, 16)}…` : "Tip —";
  byId("balance-state").textContent = status.peer_count > 0 ? "CONNECTED LOCAL CHAIN" : "OFFLINE LOCAL CHAIN";
  const address = status.addresses?.[status.addresses.length - 1] || "";
  byId("receive-address").textContent = address || "No receive address";
  byId("receive-qr").src = address ? `/api/v1/receive-qr?address=${encodeURIComponent(address)}` : "";
  byId("backup-destination").value ||= suggestedBackupPath(status.wallet_path);
  const canSend = Boolean(status.send_available);
  byId("preview-send").disabled = !canSend;
  byId("send-availability").textContent = canSend
    ? "A connected peer is available. You will review the exact payment before broadcast."
    : "Sending unlocks after the local chain has data and at least one peer is connected.";
}

async function refreshStatus({ quiet = false } = {}) {
  try {
    renderStatus(await api("/api/v1/status"));
  } catch (error) {
    byId("loading-view").hidden = true;
    if (!quiet) showToast(error.message, true);
  }
}

async function createWallet() {
  const button = byId("create-wallet");
  setBusy(button, true, "Creating safely…");
  try {
    renderStatus(await api("/api/v1/wallet/create", { method: "POST", body: "{}" }));
    showToast("Wallet created. Make your offline backup next.");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function copyAddress() {
  const address = byId("receive-address").textContent;
  try {
    await navigator.clipboard.writeText(address);
    showToast("Receive address copied.");
  } catch {
    const selection = window.getSelection();
    const range = document.createRange();
    range.selectNodeContents(byId("receive-address"));
    selection.removeAllRanges();
    selection.addRange(range);
    showToast("Address selected. Press Ctrl+C or Command+C to copy.");
  }
}

async function newAddress() {
  const button = byId("new-address");
  setBusy(button, true, "Creating…");
  try {
    await api("/api/v1/wallet/address", { method: "POST", body: "{}" });
    await refreshStatus({ quiet: true });
    showToast("New receive address created.");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function backupWallet() {
  const button = byId("backup-wallet");
  const destination = byId("backup-destination").value.trim();
  if (!destination) {
    showToast("Enter the full path for the backup file.", true);
    return;
  }
  setBusy(button, true, "Writing backup…");
  try {
    const result = await api("/api/v1/wallet/backup", {
      method: "POST",
      body: JSON.stringify({ destination }),
    });
    showToast(`Backup created at ${result.destination}`);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function validAmount(value, allowZero = false) {
  if (!/^(0|[1-9][0-9]*)(\.[0-9]{1,8})?$/.test(value)) return false;
  return allowZero || !/^0(?:\.0{1,8})?$/.test(value);
}

async function previewPayment(event) {
  event.preventDefault();
  const destination = byId("send-destination").value.trim();
  const amount = byId("send-amount").value.trim();
  const fee = byId("send-fee").value.trim();
  if (!destination || !validAmount(amount) || !validAmount(fee, true)) {
    showToast("Check the address, amount, and fee. Use up to eight decimal places.", true);
    return;
  }
  const button = byId("preview-send");
  setBusy(button, true, "Preparing locally…");
  try {
    state.pending = await api("/api/v1/send/preview", {
      method: "POST",
      body: JSON.stringify({ destination, amount, fee }),
    });
    byId("review-destination").textContent = state.pending.destination;
    byId("review-amount").textContent = `${formatCoins(state.pending.amount_units)} 09C`;
    byId("review-fee").textContent = `${formatCoins(state.pending.fee_units)} 09C`;
    byId("review-total").textContent = `${formatCoins(state.pending.total_units)} 09C`;
    byId("review-height").textContent = Number(state.pending.chain_height).toLocaleString();
    byId("review-code").textContent = state.pending.confirmation_code;
    byId("review-payment").showModal();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function confirmPayment() {
  if (!state.pending?.pending_id) return;
  const button = byId("confirm-send");
  setBusy(button, true, "Broadcasting…");
  try {
    const result = await api("/api/v1/send/confirm", {
      method: "POST",
      body: JSON.stringify({ pending_id: state.pending.pending_id }),
    });
    state.pending = null;
    byId("review-payment").close();
    byId("result-txid").textContent = result.txid;
    byId("send-result").hidden = false;
    byId("send-form").reset();
    byId("send-fee").value = "0.00010000";
    await refreshStatus({ quiet: true });
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function selectPanel(button) {
  document.querySelectorAll(".ledger-tab").forEach((tab) => {
    const active = tab === button;
    tab.classList.toggle("is-active", active);
    byId(tab.dataset.panel).hidden = !active;
  });
}

function bindEvents() {
  byId("wallet-safety-ack").addEventListener("change", (event) => { byId("create-wallet").disabled = !event.target.checked; });
  byId("create-wallet").addEventListener("click", createWallet);
  byId("copy-address").addEventListener("click", copyAddress);
  byId("new-address").addEventListener("click", newAddress);
  byId("backup-wallet").addEventListener("click", backupWallet);
  byId("send-form").addEventListener("submit", previewPayment);
  byId("confirm-send").addEventListener("click", confirmPayment);
  byId("dismiss-result").addEventListener("click", () => { byId("send-result").hidden = true; });
  document.querySelectorAll(".ledger-tab").forEach((button) => button.addEventListener("click", () => selectPanel(button)));
  byId("review-payment").addEventListener("close", () => {
    if (byId("review-payment").returnValue === "cancel") state.pending = null;
  });
}

bindEvents();
refreshStatus();
setInterval(() => refreshStatus({ quiet: true }), 15000);
