use tauri::{
    plugin::{Builder, TauriPlugin},
    Manager, Runtime,
};

pub use models::*;

#[cfg(desktop)]
mod desktop;
#[cfg(mobile)]
mod mobile;

mod commands;
mod error;
mod models;

pub use error::{Error, Result};

#[cfg(desktop)]
use desktop::WalletCore;
#[cfg(mobile)]
use mobile::WalletCore;

/// Extensions to [`tauri::App`], [`tauri::AppHandle`] and [`tauri::Window`] to access the wallet-core APIs.
pub trait WalletCoreExt<R: Runtime> {
    fn wallet_core(&self) -> &WalletCore<R>;
}

impl<R: Runtime, T: Manager<R>> crate::WalletCoreExt<R> for T {
    fn wallet_core(&self) -> &WalletCore<R> {
        self.state::<WalletCore<R>>().inner()
    }
}

/// Initializes the plugin.
pub fn init<R: Runtime>() -> TauriPlugin<R> {
    Builder::new("wallet-core")
        .invoke_handler(tauri::generate_handler![
            commands::status,
            commands::create_wallet,
            commands::restore_wallet,
            commands::unlock,
            commands::lock,
            commands::receive,
            commands::activity,
            commands::preview_send,
            commands::confirm_send,
            commands::cancel_send,
            commands::recovery_phrase,
        ])
        .setup(|app, api| {
            #[cfg(mobile)]
            let wallet_core = mobile::init(app, api)?;
            #[cfg(desktop)]
            let wallet_core = desktop::init(app, api)?;
            app.manage(wallet_core);
            Ok(())
        })
        .build()
}
