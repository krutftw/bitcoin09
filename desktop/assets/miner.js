"use strict";

state.miner = null;
state.minerPollTimer = null;

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

function setMinerAvailable(available) {
  const minerTab = document.querySelector('[data-panel="miner-panel"]');
  if (!minerTab) return;
  minerTab.hidden = !available;
  minerTab.parentElement?.classList.toggle("wallet-only", !available);
  if (!available && minerTab.classList.contains("is-active")) {
    const receiveTab = document.querySelector('[data-panel="receive-panel"]');
    if (receiveTab) selectPanel(receiveTab);
  }
}

function minerStateCopy(status) {
  if (!status?.available) return "The official miner is not available in this build.";
  if (!status.wallet_ready) return "Create your wallet before starting the miner.";
  if (status.state === "connecting") return "Connecting to the official PPLNS pool…";
  if (status.state === "mining") {
    if (status.blocks_accepted > 0) return `Block accepted at height ${Number(status.height).toLocaleString()}. Mining the next job.`;
    if (status.shares_accepted > 0) return `Share ${Number(status.last_share_sequence).toLocaleString()} accepted. Mining for the next one.`;
    return `Mining PPLNS job at height ${Number(status.height || 0).toLocaleString()}.`;
  }
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
    `Pool mode: ${status?.mining_mode || "unknown"}`,
    `Pool fee: ${Number(status?.pool_fee_bps || 0) / 100}%`,
    `Shares: ${Number(status?.shares_accepted || 0)}`,
    `Blocks: ${Number(status?.blocks_accepted || 0)}`,
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
  byId("miner-shares").textContent = Number(status.shares_accepted || 0).toLocaleString();
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
    const status = await api("/api/v1/miner/status");
    setMinerAvailable(true);
    renderMinerStatus(status);
  } catch (error) {
    clearTimeout(state.minerPollTimer);
    state.minerPollTimer = null;
    if (error.code === "miner_unavailable") setMinerAvailable(false);
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
document.querySelector('[data-panel="miner-panel"]').addEventListener("click", () => refreshMinerStatus({ quiet: true }));
refreshMinerStatus({ quiet: true });
