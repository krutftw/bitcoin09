const closeButton = document.querySelector("#close-wallet");

window.showStartError = (message) => {
  document.body.classList.add("has-error");
  document.querySelector(".eyebrow").textContent = "COULD NOT OPEN";
  document.querySelector("#title").textContent = "Wallet could not start";
  document.querySelector("#detail").textContent =
    message || "Close BTC09 Wallet, then open it again. Reinstall it if the problem continues.";
  closeButton.hidden = false;
};

closeButton.addEventListener("click", async () => {
  try {
    await window.__TAURI__.core.invoke("close_wallet");
  } catch {
    document.querySelector("#detail").textContent = "Use the close button in the window title bar.";
    closeButton.hidden = true;
  }
});
