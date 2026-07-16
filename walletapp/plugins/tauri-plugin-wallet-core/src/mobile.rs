use serde::de::DeserializeOwned;
use tauri::{
    plugin::{PluginApi, PluginHandle},
    AppHandle, Runtime,
};

use crate::{
    ActivityRequest, EmptyRequest, PendingRequest, PreviewSendRequest, RestoreRequest,
    WalletResponse,
};

#[cfg(target_os = "ios")]
tauri::ios_plugin_binding!(init_plugin_wallet_core);

pub fn init<R: Runtime, C: DeserializeOwned>(
    _app: &AppHandle<R>,
    api: PluginApi<R, C>,
) -> crate::Result<WalletCore<R>> {
    #[cfg(target_os = "android")]
    let handle = api.register_android_plugin("org.bitcoin09.walletcore", "WalletCorePlugin")?;
    #[cfg(target_os = "ios")]
    let handle = api.register_ios_plugin(init_plugin_wallet_core)?;
    Ok(WalletCore(handle))
}

pub struct WalletCore<R: Runtime>(PluginHandle<R>);

impl<R: Runtime> WalletCore<R> {
    fn empty(&self, command: &str) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin(command, EmptyRequest {})
            .map_err(Into::into)
    }

    pub fn status(&self) -> crate::Result<WalletResponse> {
        self.empty("status")
    }
    pub fn create_wallet(&self) -> crate::Result<WalletResponse> {
        self.empty("createWallet")
    }
    pub fn restore_wallet(&self, payload: RestoreRequest) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin("restoreWallet", payload)
            .map_err(Into::into)
    }
    pub fn unlock(&self) -> crate::Result<WalletResponse> {
        self.empty("unlock")
    }
    pub fn lock(&self) -> crate::Result<WalletResponse> {
        self.empty("lock")
    }
    pub fn receive(&self) -> crate::Result<WalletResponse> {
        self.empty("receive")
    }
    pub fn activity(&self, payload: ActivityRequest) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin("activity", payload)
            .map_err(Into::into)
    }
    pub fn preview_send(&self, payload: PreviewSendRequest) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin("previewSend", payload)
            .map_err(Into::into)
    }
    pub fn confirm_send(&self, payload: PendingRequest) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin("confirmSend", payload)
            .map_err(Into::into)
    }
    pub fn cancel_send(&self, payload: PendingRequest) -> crate::Result<WalletResponse> {
        self.0
            .run_mobile_plugin("cancelSend", payload)
            .map_err(Into::into)
    }
    pub fn recovery_phrase(&self) -> crate::Result<WalletResponse> {
        self.empty("recoveryPhrase")
    }
}
