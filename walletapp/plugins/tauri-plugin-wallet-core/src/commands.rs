use tauri::{command, AppHandle, Runtime};

use crate::{ActivityRequest, PendingRequest, PreviewSendRequest, RestoreRequest};
use crate::{Result, WalletCoreExt};

#[command]
pub(crate) async fn status<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().status()?.data)
}

#[command]
pub(crate) async fn create_wallet<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().create_wallet()?.data)
}

#[command]
pub(crate) async fn restore_wallet<R: Runtime>(
    app: AppHandle<R>,
    payload: RestoreRequest,
) -> Result<String> {
    Ok(app.wallet_core().restore_wallet(payload)?.data)
}

#[command]
pub(crate) async fn unlock<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().unlock()?.data)
}

#[command]
pub(crate) async fn lock<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().lock()?.data)
}

#[command]
pub(crate) async fn receive<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().receive()?.data)
}

#[command]
pub(crate) async fn activity<R: Runtime>(
    app: AppHandle<R>,
    payload: ActivityRequest,
) -> Result<String> {
    Ok(app.wallet_core().activity(payload)?.data)
}

#[command]
pub(crate) async fn preview_send<R: Runtime>(
    app: AppHandle<R>,
    payload: PreviewSendRequest,
) -> Result<String> {
    Ok(app.wallet_core().preview_send(payload)?.data)
}

#[command]
pub(crate) async fn confirm_send<R: Runtime>(
    app: AppHandle<R>,
    payload: PendingRequest,
) -> Result<String> {
    Ok(app.wallet_core().confirm_send(payload)?.data)
}

#[command]
pub(crate) async fn cancel_send<R: Runtime>(
    app: AppHandle<R>,
    payload: PendingRequest,
) -> Result<String> {
    Ok(app.wallet_core().cancel_send(payload)?.data)
}

#[command]
pub(crate) async fn recovery_phrase<R: Runtime>(app: AppHandle<R>) -> Result<String> {
    Ok(app.wallet_core().recovery_phrase()?.data)
}
