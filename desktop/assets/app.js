"use strict";

const state = {
  csrf: "",
  status: null,
  pending: null,
  toastTimer: null,
  miner: null,
  minerPollTimer: null,
  recoveryPhrase: null,
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
	const fastMode = status.mode === "fast";
	byId("wallet-mode").textContent = fastMode ? "FAST MODE" : "FULL NODE";
  byId("chain-height").textContent = Number(status.height || 0).toLocaleString();
  byId("sync-state").textContent = status.sync_state || "offline";
	const connected = status.sync_state === "connected";
	byId("status-lamp").classList.toggle("is-online", connected);
	byId("status-lamp").classList.toggle("is-ready", !connected && status.sync_state === "ready");
  byId("loading-view").hidden = true;
  const showingRecovery = Boolean(state.recoveryPhrase);
  byId("first-run").hidden = status.wallet_exists || showingRecovery;
  byId("locked-view").hidden = !status.wallet_exists || !status.needs_unlock || showingRecovery;
  byId("recovery-backup").hidden = !showingRecovery;
  byId("wallet-view").hidden = !status.wallet_exists || status.needs_unlock || showingRecovery;
  byId("first-run-path").textContent = status.wallet_path || "—";

  if (!status.wallet_exists || status.needs_unlock) {
    clearTimeout(state.minerPollTimer);
    state.minerPollTimer = null;
    return;
  }
	const balanceAvailable = Boolean(status.balance_available);
	const formatted = formatCoins(status.balance_units).split(".");
	byId("balance-major").textContent = balanceAvailable ? Number(formatted[0]).toLocaleString() : "—";
	byId("balance-minor").textContent = balanceAvailable ? `.${formatted[1]}` : "";
  byId("tip-hash").textContent = status.tip_hash ? `Tip ${status.tip_hash.slice(0, 16)}…` : "Tip —";
	byId("balance-state").textContent = !balanceAvailable
	  ? "BALANCE TEMPORARILY UNAVAILABLE"
	  : (fastMode ? "FAST MODE · SIGNING ON THIS DEVICE" : (status.peer_count > 0 ? "FULL NODE · CONNECTED" : "FULL NODE · OFFLINE"));
  const address = status.addresses?.[status.addresses.length - 1] || "";
  byId("receive-address").textContent = address || "No receive address";
  byId("address-chip-text").textContent = address ? `${address.slice(0, 7)}…${address.slice(-5)}` : "No address";
  byId("receive-qr").src = address ? `/api/v1/receive-qr?address=${encodeURIComponent(address)}` : "";
  const recoveryWallet = status.wallet_version === 2;
  byId("new-address").hidden = recoveryWallet;
  byId("receive-note").textContent = recoveryWallet
    ? "This stable address is recovered from your 24 words. Never share the words themselves."
    : "A new address still belongs to this wallet. Never share the wallet file itself.";
  byId("show-recovery-form").hidden = !recoveryWallet;
  byId("backup-intro").textContent = recoveryWallet
    ? "Your recovery words are the main backup. An encrypted file copy is useful for this device too."
    : "Save a copy to a disconnected USB drive or another offline location.";
  byId("backup-destination").value ||= suggestedBackupPath(status.wallet_path);
  const canSend = Boolean(status.send_available);
  byId("preview-send").disabled = !canSend;
  byId("send-availability").textContent = canSend
	  ? (fastMode
	    ? "Ready. The payment is built and signed on this computer, then sent for relay."
	    : "A connected peer is available. You will review the exact payment before broadcast.")
	  : (fastMode
	    ? "Wallet service is temporarily unavailable. Your funds are safe; try again."
	    : "Sending unlocks after the local chain has data and at least one peer is connected.");
  refreshMinerStatus({ quiet: true });
}

async function refreshStatus({ quiet = false } = {}) {
  try {
    renderStatus(await api("/api/v1/status"));
  } catch (error) {
    byId("loading-view").hidden = true;
    if (!quiet) showToast(error.message, true);
  }
}

function switchSetup(mode) {
  const restoring = mode === "restore";
  byId("create-wallet-form").hidden = restoring;
  byId("restore-wallet-form").hidden = !restoring;
  byId("show-create").classList.toggle("is-active", !restoring);
  byId("show-restore").classList.toggle("is-active", restoring);
}

function matchingPassword(firstID, secondID) {
  const password = byId(firstID).value;
  if (password.length < 12) {
    showToast("Use at least 12 characters for the wallet password.", true);
    return null;
  }
  if (password !== byId(secondID).value) {
    showToast("The two passwords do not match.", true);
    return null;
  }
  return password;
}

function renderRecoveryPhrase(phrase) {
  const words = phrase.split(" ");
  if (words.length !== 24) throw new Error("BTC09 returned an invalid recovery phrase.");
  const list = byId("recovery-word-grid");
  list.replaceChildren();
  words.forEach((word) => {
    const item = document.createElement("li");
    const value = document.createElement("span");
    value.textContent = word;
    item.append(value);
    list.append(item);
  });
  byId("recovery-write-step").hidden = false;
  byId("recovery-confirm-step").hidden = true;
}

async function createWallet(event) {
  event.preventDefault();
  if (!byId("wallet-safety-ack").checked) {
    showToast("Confirm that you understand the recovery responsibility.", true);
    return;
  }
  const password = matchingPassword("create-password", "create-password-confirm");
  if (password === null) return;
  const button = byId("create-wallet");
  setBusy(button, true, "Creating safely…");
  try {
    const result = await api("/api/v1/wallet/v2/create", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    state.recoveryPhrase = result.recovery_phrase;
    renderRecoveryPhrase(state.recoveryPhrase);
    renderStatus(result.status);
    byId("create-wallet-form").reset();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function restoreWallet(event) {
  event.preventDefault();
  const password = matchingPassword("restore-password", "restore-password-confirm");
  if (password === null) return;
  const recoveryPhrase = byId("restore-phrase").value.trim().split(/\s+/).join(" ").toLowerCase();
  if (recoveryPhrase.split(" ").length !== 24) {
    showToast("Enter all 24 recovery words in order.", true);
    return;
  }
  const button = byId("restore-wallet");
  setBusy(button, true, "Restoring…");
  try {
    renderStatus(await api("/api/v1/wallet/v2/restore", {
      method: "POST",
      body: JSON.stringify({ password, recovery_phrase: recoveryPhrase }),
    }));
    byId("restore-wallet-form").reset();
    showToast("Wallet restored and encrypted on this device.");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function unlockWallet(event) {
  event.preventDefault();
  const button = byId("unlock-wallet");
  const password = byId("unlock-password").value;
  if (!password) return;
  setBusy(button, true, "Unlocking…");
  try {
    renderStatus(await api("/api/v1/wallet/v2/unlock", {
      method: "POST",
      body: JSON.stringify({ password }),
    }));
    byId("unlock-wallet-form").reset();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function startRecoveryConfirmation() {
  byId("recovery-write-step").hidden = true;
  byId("recovery-confirm-step").hidden = false;
  byId("confirm-word-4").focus();
}

function showRecoveryWordsAgain() {
  byId("recovery-confirm-step").reset();
  byId("recovery-confirm-step").hidden = true;
  byId("recovery-write-step").hidden = false;
}

function confirmRecoveryBackup(event) {
  event.preventDefault();
  const words = state.recoveryPhrase?.split(" ") || [];
  const checks = [
    [3, "confirm-word-4"],
    [11, "confirm-word-12"],
    [20, "confirm-word-21"],
  ];
  const matches = checks.every(([index, id]) => byId(id).value.trim().toLowerCase() === words[index]);
  if (!matches) {
    showToast("One or more words do not match. Check your written backup.", true);
    return;
  }
  byId("recovery-confirm-step").reset();
  byId("recovery-word-grid").replaceChildren();
  state.recoveryPhrase = null;
  renderStatus(state.status);
  showToast("Recovery backup checked. Your wallet is ready.");
}

async function showRecoveryPhrase(event) {
  event.preventDefault();
  const button = byId("show-recovery-phrase");
  const password = byId("recovery-password").value;
  if (!password) return;
  setBusy(button, true, "Checking…");
  try {
    const result = await api("/api/v1/wallet/v2/recovery", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    byId("show-recovery-form").reset();
    state.recoveryPhrase = result.recovery_phrase;
    renderRecoveryPhrase(state.recoveryPhrase);
    renderStatus(state.status);
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

function formatHashrate(value) {
  const rate = Number(value || 0);
  if (!Number.isFinite(rate) || rate <= 0) return "0.00 H/s";
  if (rate >= 1000000) return `${(rate / 1000000).toFixed(2)} MH/s`;
  if (rate >= 1000) return `${(rate / 1000).toFixed(2)} KH/s`;
  return `${rate.toFixed(2)} H/s`;
}

function formatElapsed(seconds) {
  const value = Math.max(0, Number(seconds || 0));
  if (value < 60) return `${Math.floor(value)}s`;
  if (value < 3600) return `${Math.floor(value / 60)}m ${Math.floor(value % 60)}s`;
  return `${Math.floor(value / 3600)}h ${Math.floor((value % 3600) / 60)}m`;
}

function minerIsActive(status) {
  return ["connecting", "mining", "retrying", "stopping"].includes(status?.state);
}

function minerStateCopy(status) {
  if (!status?.available) return "The official miner is not available in this build.";
  if (!status.wallet_ready) return "Create your wallet before starting the miner.";
  if (status.state === "connecting") return "Connecting to the official Open solo endpoint…";
  if (status.state === "mining") return status.blocks_accepted > 0
    ? `Block accepted at height ${Number(status.height).toLocaleString()}. Mining the next job.`
    : `Mining job at height ${Number(status.height || 0).toLocaleString()}.`;
  if (status.state === "retrying") return `${status.last_error || "Connection interrupted."} Retrying in ${status.retry_in_seconds || 1}s.`;
  if (status.state === "stopping") return "Stopping after the current hash attempt…";
  if (status.state === "error") return status.last_error || "Mining stopped after an endpoint error.";
  return "Ready when you are.";
}

function minerEndpointCheck(status) {
  if (!status?.available) return "Unavailable";
  if (status.state === "mining") return "Connected";
  if (status.state === "connecting") return "Checking";
  if (status.state === "retrying") return "Retrying";
  if (status.state === "error") return "Needs attention";
  return status.jobs > 0 ? "Last check passed" : "Not tested";
}

function minerSupportReport(status) {
  const logicalCPUs = Math.max(1, Number(status?.logical_cpus || 1));
  const selectedWorkers = Math.max(1, Number(status?.workers || byId("miner-workers")?.value || 1));
  return [
    "BTC09 miner help report",
    `Version: ${state.status?.version || "unknown"}`,
    `Network: ${state.status?.network || "unknown"}`,
    `Wallet mode: ${state.status?.mode || "unknown"}`,
    `Miner state: ${status?.state || "unknown"}`,
    `CPU threads: ${selectedWorkers} of ${logicalCPUs}`,
    `Current rate: ${formatHashrate(status?.current_hashrate)}`,
    `Session average: ${formatHashrate(status?.average_hashrate)}`,
    `Jobs: ${Number(status?.jobs || 0)}`,
    `Reconnects: ${Number(status?.reconnects || 0)}`,
    `Session time: ${formatElapsed(status?.elapsed_seconds)}`,
    `Job height: ${Number(status?.height || 0) || "none"}`,
    `Last error: ${status?.last_error || "none"}`,
  ].join("\n");
}

async function copyMinerReport() {
  if (!state.miner) await refreshMinerStatus({ quiet: false });
  if (!state.miner) return;
  try {
    await navigator.clipboard.writeText(minerSupportReport(state.miner));
    showToast("Miner help report copied. It leaves out your wallet address and worker name.");
  } catch {
    showToast("The help report could not be copied. Check clipboard permission and try again.", true);
  }
}

function renderMinerStatus(status) {
  state.miner = status;
  const active = minerIsActive(status);
  const logicalCPUs = Math.max(1, Number(status.logical_cpus || 1));
  const workers = byId("miner-workers");
  workers.max = String(logicalCPUs);
  if (!workers.dataset.ready || active) {
    const suggested = Number(status.workers || Math.max(1, logicalCPUs - 1));
    workers.value = String(Math.min(logicalCPUs, Math.max(1, suggested)));
    workers.dataset.ready = "true";
  }
  byId("miner-workers-value").textContent = `${workers.value} of ${logicalCPUs}`;
  byId("miner-wallet-check").textContent = status.wallet_ready ? "Ready" : "Create wallet";
  byId("miner-endpoint-check").textContent = minerEndpointCheck(status);
  byId("miner-endpoint-check").dataset.state = status.state || "stopped";
  byId("miner-cpu-check").textContent = `${workers.value} of ${logicalCPUs}`;
  const freeCPUs = Math.max(0, logicalCPUs - Number(workers.value));
  byId("miner-cpu-guidance").textContent = freeCPUs > 0
    ? `${freeCPUs} thread${freeCPUs === 1 ? "" : "s"} left free so this computer stays responsive.`
    : `Using every thread may slow this computer. Try ${Math.max(1, logicalCPUs - 1)} for everyday use.`;
  const fallbackAddress = state.status?.addresses?.[0] || "";
  byId("miner-address").textContent = status.address || fallbackAddress || "Create a wallet first";
  byId("miner-current-hashrate").textContent = formatHashrate(status.current_hashrate);
  byId("miner-average-hashrate").textContent = formatHashrate(status.average_hashrate);
  byId("miner-total-hashes").textContent = Number(status.total_hashes || 0).toLocaleString();
  byId("miner-blocks").textContent = Number(status.blocks_accepted || 0).toLocaleString();
  byId("miner-state").textContent = (status.state || "stopped").replace(/^./, (letter) => letter.toUpperCase());
  byId("miner-state").dataset.state = status.state || "stopped";
  byId("miner-state-line").textContent = minerStateCopy(status);
  byId("miner-session-meta").textContent = `${Number(status.jobs || 0).toLocaleString()} jobs · ${Number(status.reconnects || 0).toLocaleString()} reconnects · ${formatElapsed(status.elapsed_seconds)}`;
  byId("start-miner").disabled = active || !status.available || !status.wallet_ready;
  byId("stop-miner").disabled = !active || status.state === "stopping";
  workers.disabled = active;
  byId("miner-worker").disabled = active;

  clearTimeout(state.minerPollTimer);
  state.minerPollTimer = null;
  if (active) state.minerPollTimer = setTimeout(refreshMinerStatus, 1000);
}

async function refreshMinerStatus({ quiet = true } = {}) {
  try {
    renderMinerStatus(await api("/api/v1/miner/status"));
  } catch (error) {
    clearTimeout(state.minerPollTimer);
    state.minerPollTimer = null;
    if (!quiet) showToast(error.message, true);
  }
}

async function startMiner(event) {
  event.preventDefault();
  const worker = byId("miner-worker").value.trim();
  const workers = Number(byId("miner-workers").value);
  if (worker && !/^[A-Za-z0-9._-]{1,64}$/.test(worker)) {
    showToast("Worker names can use letters, numbers, dots, dashes, and underscores.", true);
    return;
  }
  const button = byId("start-miner");
  setBusy(button, true, "Connecting…");
  try {
    renderMinerStatus(await api("/api/v1/miner/start", {
      method: "POST",
      body: JSON.stringify({ workers, worker }),
    }));
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
    if (state.miner) renderMinerStatus(state.miner);
  }
}

async function stopMiner() {
  const button = byId("stop-miner");
  setBusy(button, true, "Stopping…");
  try {
    renderMinerStatus(await api("/api/v1/miner/stop", { method: "POST", body: "{}" }));
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
    if (state.miner) renderMinerStatus(state.miner);
  }
}

function selectPanel(button) {
  document.querySelectorAll(".ledger-tab").forEach((tab) => {
    const active = tab === button;
    tab.classList.toggle("is-active", active);
    byId(tab.dataset.panel).hidden = !active;
  });
  if (button.dataset.panel === "miner-panel") refreshMinerStatus({ quiet: true });
}

function bindEvents() {
  byId("show-create").addEventListener("click", () => switchSetup("create"));
  byId("show-restore").addEventListener("click", () => switchSetup("restore"));
  byId("create-wallet-form").addEventListener("submit", createWallet);
  byId("restore-wallet-form").addEventListener("submit", restoreWallet);
  byId("unlock-wallet-form").addEventListener("submit", unlockWallet);
  byId("start-recovery-confirm").addEventListener("click", startRecoveryConfirmation);
  byId("show-recovery-words").addEventListener("click", showRecoveryWordsAgain);
  byId("recovery-confirm-step").addEventListener("submit", confirmRecoveryBackup);
  byId("show-recovery-form").addEventListener("submit", showRecoveryPhrase);
  byId("copy-address").addEventListener("click", copyAddress);
  byId("new-address").addEventListener("click", newAddress);
  byId("backup-wallet").addEventListener("click", backupWallet);
  byId("send-form").addEventListener("submit", previewPayment);
  byId("miner-form").addEventListener("submit", startMiner);
  byId("stop-miner").addEventListener("click", stopMiner);
  byId("copy-miner-report").addEventListener("click", copyMinerReport);
  byId("miner-workers").addEventListener("input", (event) => {
    byId("miner-workers-value").textContent = `${event.target.value} of ${event.target.max}`;
    byId("miner-cpu-check").textContent = `${event.target.value} of ${event.target.max}`;
    const freeCPUs = Math.max(0, Number(event.target.max) - Number(event.target.value));
    byId("miner-cpu-guidance").textContent = freeCPUs > 0
      ? `${freeCPUs} thread${freeCPUs === 1 ? "" : "s"} left free so this computer stays responsive.`
      : `Using every thread may slow this computer. Try ${Math.max(1, Number(event.target.max) - 1)} for everyday use.`;
  });
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
