use serde::de::DeserializeOwned;
use tauri::{plugin::PluginApi, AppHandle, Runtime};

use crate::{
    ActivityRequest, Error, PendingRequest, PreviewSendRequest, RestoreRequest, WalletResponse,
};

pub fn init<R: Runtime, C: DeserializeOwned>(
    app: &AppHandle<R>,
    _api: PluginApi<R, C>,
) -> crate::Result<WalletCore<R>> {
    Ok(WalletCore(app.clone()))
}

pub struct WalletCore<R: Runtime>(#[allow(dead_code)] AppHandle<R>);

impl<R: Runtime> WalletCore<R> {
    pub fn status(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn create_wallet(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn restore_wallet(&self, _: RestoreRequest) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn unlock(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn lock(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn receive(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn activity(&self, _: ActivityRequest) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn preview_send(&self, _: PreviewSendRequest) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn confirm_send(&self, _: PendingRequest) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn cancel_send(&self, _: PendingRequest) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
    pub fn recovery_phrase(&self) -> crate::Result<WalletResponse> {
        Err(Error::Unsupported)
    }
}
