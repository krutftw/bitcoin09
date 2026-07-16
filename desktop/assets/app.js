"use strict";

const explorerTxBase = "https://explorer.btc09.org/tx/";
const walletLockDelay = 5 * 60 * 1000;

const state = {
  csrf: "",
  status: null,
  pending: null,
  cleanupPending: null,
  toastTimer: null,
  activityPollTimer: null,
  recoveryPhrase: null,
  lockTimer: null,
  locking: false,
};

const byId = (id) => document.getElementById(id);

async function api(path, options = {}) {
  return BTC09Network.request(path, options, { csrf: () => state.csrf });
}

function formatCoins(units) {
  const value = BigInt(units || 0);
  const whole = value / 100000000n;
  const fraction = (value % 100000000n).toString().padStart(8, "0");
  return `${whole}.${fraction}`;
}

function formatSignedCoins(units) {
  const value = BigInt(units || 0);
  const magnitude = value < 0n ? -value : value;
  const whole = magnitude / 100000000n;
  const fraction = (magnitude % 100000000n).toString().padStart(8, "0");
  const sign = value > 0n ? "+" : value < 0n ? "−" : "";
  return `${sign}${whole}.${fraction}`;
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
  const recoveryWallet = status.wallet_version === 2;
  byId("lock-wallet").hidden = !recoveryWallet;

  if (!status.wallet_exists || status.needs_unlock) {
    clearTimeout(state.lockTimer);
    state.lockTimer = null;
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
  const immatureUnits = Number(status.immature_units || 0);
  byId("mining-rewards").hidden = immatureUnits <= 0;
  byId("mining-rewards-value").textContent = `${formatCoins(immatureUnits)} 09C`;
  const address = status.addresses?.[status.addresses.length - 1] || "";
  byId("receive-address").textContent = address || "No receive address";
  byId("address-chip-text").textContent = address ? `${address.slice(0, 7)}…${address.slice(-5)}` : "No address";
  byId("receive-qr").src = address ? `/api/v1/receive-qr?address=${encodeURIComponent(address)}` : "";
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
  byId("send-max").disabled = !canSend;
  byId("send-availability").textContent = canSend
	  ? (fastMode
	    ? "Ready. The payment is built and signed on this computer, then sent for relay."
	    : "A connected peer is available. You will review the exact payment before broadcast.")
	  : (fastMode
	    ? "Wallet service is temporarily unavailable. Your funds are safe; try again."
	    : "Sending unlocks after the local chain has data and at least one peer is connected.");
  const cleanupAvailable = Boolean(status.cleanup_available);
  const cleanupRecommended = Boolean(status.cleanup_recommended);
  byId("cleanup-card").hidden = !cleanupAvailable;
  byId("cleanup-card").classList.toggle("is-recommended", cleanupRecommended);
  byId("cleanup-summary").textContent = cleanupRecommended
    ? `${Number(status.spendable_output_count || 0).toLocaleString()} small payments are making sends heavier. Combining them now will help.`
    : "Combine small payments now to make a future send simpler.";
  ensureLockTimer();
}

async function refreshStatus({ quiet = false } = {}) {
  try {
    renderStatus(await api("/api/v1/status"));
  } catch (error) {
    byId("loading-view").hidden = true;
    if (!quiet) showToast(error.message, true);
  }
}

function clearSensitiveDesktopState() {
  state.recoveryPhrase = null;
  state.pending = null;
  state.cleanupPending = null;
  byId("recovery-word-grid").replaceChildren();
  byId("recovery-confirm-step").reset();
  byId("create-wallet-form").reset();
  byId("restore-wallet-form").reset();
  byId("unlock-wallet-form").reset();
  byId("show-recovery-form").reset();
  byId("send-form").reset();
  for (const id of ["review-payment", "review-cleanup"]) {
    const dialog = byId(id);
    if (dialog.open) dialog.close();
  }
}

function ensureLockTimer() {
  if (state.lockTimer !== null || !state.status?.wallet_exists || state.status.needs_unlock || state.status.wallet_version !== 2 || state.locking) return;
  state.lockTimer = setTimeout(() => { void lockWallet({ quiet: true }); }, walletLockDelay);
}

function resetLockTimer() {
  clearTimeout(state.lockTimer);
  state.lockTimer = null;
  ensureLockTimer();
}

async function lockWallet({ quiet = false } = {}) {
  clearSensitiveDesktopState();
  if (state.locking || !state.status?.wallet_exists || state.status.needs_unlock || state.status.wallet_version !== 2) return;
  state.locking = true;
  clearTimeout(state.lockTimer);
  state.lockTimer = null;
  try {
    const status = await api("/api/v1/wallet/v2/lock", { method: "POST", body: "{}" });
    renderStatus(status);
    if (!quiet) showToast("Wallet locked.");
  } catch (error) {
    if (!quiet) showToast(error.message, true);
    await refreshStatus({ quiet: true });
  } finally {
    state.locking = false;
    ensureLockTimer();
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

function showPaymentReview(preview) {
  state.pending = preview;
  byId("review-destination").textContent = preview.destination;
  byId("review-amount").textContent = `${formatCoins(preview.amount_units)} 09C`;
  byId("review-fee").textContent = `${formatCoins(preview.fee_units)} 09C`;
  byId("review-total").textContent = `${formatCoins(preview.total_units)} 09C`;
  byId("review-height").textContent = Number(preview.chain_height).toLocaleString();
  byId("review-code").textContent = preview.confirmation_code;
  const hasSelectedInputs = Array.isArray(preview.selected_inputs);
  const inputCount = hasSelectedInputs ? preview.selected_inputs.length : 0;
  byId("review-input-count").hidden = inputCount <= 1;
  byId("review-input-count-value").textContent = `${inputCount.toLocaleString()} payments`;
  byId("review-payment").showModal();
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
    const preview = await api("/api/v1/send/preview", {
      method: "POST",
      body: JSON.stringify({ destination, amount, fee }),
    });
    showPaymentReview(preview);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function previewMaxPayment() {
  const destination = byId("send-destination").value.trim();
  const fee = byId("send-fee").value.trim();
  if (!destination || !validAmount(fee, true)) {
    showToast("Enter the destination and check the fee first.", true);
    return;
  }
  const button = byId("send-max");
  setBusy(button, true, "Working…");
  try {
    const preview = await api("/api/v1/send/max-preview", {
      method: "POST",
      body: JSON.stringify({ destination, fee }),
    });
    byId("send-amount").value = formatCoins(preview.amount_units);
    showPaymentReview(preview);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function showTransactionResult(txid, title) {
  byId("result-title").textContent = title;
  byId("result-txid").textContent = txid;
  byId("open-result-txid").href = `${explorerTxBase}${encodeURIComponent(txid)}`;
  byId("send-result").hidden = false;
}

async function copyText(value, successMessage) {
  try {
    await navigator.clipboard.writeText(value);
    showToast(successMessage);
  } catch {
    showToast("Clipboard access is blocked. Select and copy it manually.", true);
  }
}

async function cancelPendingPreview(preview) {
  if (!preview?.pending_id) return;
  try {
    await api("/api/v1/preview/cancel", {
      method: "POST",
      body: JSON.stringify({ pending_id: preview.pending_id }),
    });
  } catch {
    showToast("That review will expire on its own in a few minutes.", true);
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
    showTransactionResult(result.txid, "Payment submitted");
    byId("send-form").reset();
    byId("send-fee").value = "0.00010000";
    await refreshStatus({ quiet: true });
    if (!byId("activity-panel").hidden) refreshActivity({ quiet: true });
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

function activityPresentation(item) {
  const presentations = {
    received: { label: "Received", mark: "↓" },
    sent: { label: "Sent", mark: "↑" },
    mining_reward: { label: "Mining reward", mark: "M" },
    cleanup: { label: "Wallet cleanup", mark: "↺" },
  };
  return presentations[item.kind] || { label: "Transaction", mark: "·" };
}

function activityStatus(item) {
  if (item.status === "pending") return "Pending";
  if (item.kind === "mining_reward" && Number(item.blocks_until_mature || 0) > 0) {
    return `Ready in ${Number(item.blocks_until_mature).toLocaleString()} blocks`;
  }
  const confirmations = Number(item.confirmations || 0);
  if (confirmations > 0) return `${confirmations.toLocaleString()} confirmation${confirmations === 1 ? "" : "s"}`;
  if (Number(item.block_height || 0) > 0) return `Block ${Number(item.block_height).toLocaleString()}`;
  return "Confirmed";
}

function renderActivity(result) {
  const list = byId("activity-list");
  list.replaceChildren();
  const items = Array.isArray(result?.items) ? result.items : [];
  if (items.length === 0) {
    const empty = document.createElement("p");
    empty.className = "activity-empty";
    empty.textContent = "No wallet activity yet.";
    list.append(empty);
    return;
  }

  items.forEach((item) => {
    const presentation = activityPresentation(item);
    const row = document.createElement("article");
    row.className = "activity-row";

    const main = document.createElement("div");
    main.className = "activity-main";
    const mark = document.createElement("span");
    mark.className = "activity-mark";
    mark.textContent = presentation.mark;
    mark.setAttribute("aria-hidden", "true");
    const copy = document.createElement("div");
    copy.className = "activity-copy";
    const label = document.createElement("strong");
    label.textContent = presentation.label;
    const txid = document.createElement("code");
    txid.textContent = item.txid;
    copy.append(label, txid);
    main.append(mark, copy);

    const value = document.createElement("div");
    value.className = "activity-value";
    const amount = document.createElement("strong");
    amount.textContent = `${formatSignedCoins(item.net_units)} 09C`;
    amount.classList.toggle("is-positive", Number(item.net_units || 0) > 0);
    const status = document.createElement("span");
    status.textContent = activityStatus(item);
    value.append(amount, status);

    const actions = document.createElement("div");
    actions.className = "activity-actions";
    const copyButton = document.createElement("button");
    copyButton.type = "button";
    copyButton.textContent = "Copy TXID";
    copyButton.addEventListener("click", () => copyText(item.txid, "TXID copied."));
    const explorerLink = document.createElement("a");
    explorerLink.href = `${explorerTxBase}${encodeURIComponent(item.txid)}`;
    explorerLink.target = "_blank";
    explorerLink.rel = "noopener noreferrer";
    explorerLink.textContent = "Open in explorer ↗";
    actions.append(copyButton, explorerLink);

    row.append(main, value, actions);
    list.append(row);
  });
}

async function refreshActivity({ quiet = false } = {}) {
  clearTimeout(state.activityPollTimer);
  state.activityPollTimer = null;
  const button = byId("refresh-activity");
  if (!quiet) setBusy(button, true, "Refreshing…");
  try {
    renderActivity(await api("/api/v1/activity"));
  } catch (error) {
    if (!quiet) showToast(error.message, true);
  } finally {
    if (!quiet) setBusy(button, false);
    if (!byId("activity-panel").hidden) state.activityPollTimer = setTimeout(refreshActivity, 30000);
  }
}

async function previewCleanup() {
  const fee = byId("cleanup-fee").value.trim();
  if (!validAmount(fee, true)) {
    showToast("Check the cleanup fee. Use up to eight decimal places.", true);
    return;
  }
  const button = byId("preview-cleanup");
  setBusy(button, true, "Preparing…");
  try {
    state.cleanupPending = await api("/api/v1/maintenance/cleanup/preview", {
      method: "POST",
      body: JSON.stringify({ fee }),
    });
    byId("cleanup-review-count").textContent = `${Number(state.cleanupPending.input_count).toLocaleString()} payments`;
    byId("cleanup-review-address").textContent = state.cleanupPending.address;
    byId("cleanup-review-amount").textContent = `${formatCoins(state.cleanupPending.amount_units)} 09C`;
    byId("cleanup-review-fee").textContent = `${formatCoins(state.cleanupPending.fee_units)} 09C`;
    byId("cleanup-review-height").textContent = Number(state.cleanupPending.chain_height).toLocaleString();
    byId("cleanup-review-code").textContent = state.cleanupPending.confirmation_code;
    byId("cleanup-more-note").hidden = !state.cleanupPending.more_available;
    byId("review-cleanup").showModal();
  } catch (error) {
    showToast(error.message, true);
  } finally {
    setBusy(button, false);
  }
}

async function confirmCleanup() {
  if (!state.cleanupPending?.pending_id) return;
  const button = byId("confirm-cleanup");
  setBusy(button, true, "Broadcasting…");
  try {
    const result = await api("/api/v1/maintenance/cleanup/confirm", {
      method: "POST",
      body: JSON.stringify({ pending_id: state.cleanupPending.pending_id }),
    });
    state.cleanupPending = null;
    byId("review-cleanup").close();
    showTransactionResult(result.txid, "Wallet cleanup submitted");
    await refreshStatus({ quiet: true });
    await refreshActivity({ quiet: true });
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
  clearTimeout(state.activityPollTimer);
  state.activityPollTimer = null;
  if (button.dataset.panel === "activity-panel") refreshActivity();
}

function bindEvents() {
  byId("show-create").addEventListener("click", () => switchSetup("create"));
  byId("show-restore").addEventListener("click", () => switchSetup("restore"));
  byId("create-wallet-form").addEventListener("submit", createWallet);
  byId("restore-wallet-form").addEventListener("submit", restoreWallet);
  byId("unlock-wallet-form").addEventListener("submit", unlockWallet);
  byId("lock-wallet").addEventListener("click", () => { void lockWallet(); });
  byId("start-recovery-confirm").addEventListener("click", startRecoveryConfirmation);
  byId("show-recovery-words").addEventListener("click", showRecoveryWordsAgain);
  byId("recovery-confirm-step").addEventListener("submit", confirmRecoveryBackup);
  byId("show-recovery-form").addEventListener("submit", showRecoveryPhrase);
  byId("copy-address").addEventListener("click", copyAddress);
  byId("new-address").addEventListener("click", newAddress);
  byId("backup-wallet").addEventListener("click", backupWallet);
  byId("send-form").addEventListener("submit", previewPayment);
  byId("send-max").addEventListener("click", previewMaxPayment);
  byId("refresh-activity").addEventListener("click", () => refreshActivity());
  byId("preview-cleanup").addEventListener("click", previewCleanup);
  byId("confirm-send").addEventListener("click", confirmPayment);
  byId("confirm-cleanup").addEventListener("click", confirmCleanup);
  byId("copy-result-txid").addEventListener("click", () => copyText(byId("result-txid").textContent, "TXID copied."));
  byId("dismiss-result").addEventListener("click", () => { byId("send-result").hidden = true; });
  document.querySelectorAll(".ledger-tab").forEach((button) => button.addEventListener("click", () => selectPanel(button)));
  byId("review-payment").addEventListener("close", () => {
    const pending = state.pending;
    state.pending = null;
    cancelPendingPreview(pending);
  });
  byId("review-cleanup").addEventListener("close", () => {
    const pending = state.cleanupPending;
    state.cleanupPending = null;
    cancelPendingPreview(pending);
  });
  for (const eventName of ["pointerdown", "keydown"]) {
    window.addEventListener(eventName, resetLockTimer, { passive: true });
  }
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") {
      void lockWallet({ quiet: true });
    } else {
      void refreshStatus({ quiet: true });
    }
  });
}

bindEvents();
refreshStatus();
setInterval(() => refreshStatus({ quiet: true }), 15000);
