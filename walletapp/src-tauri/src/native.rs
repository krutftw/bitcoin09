use crate::{desktop_core_arguments, navigation_allowed, sidecar_path, BackendProcess};
use std::{
    path::PathBuf,
    process::Command,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    thread,
    time::Duration,
};
use tauri::{
    plugin::TauriPlugin,
    webview::{PageLoadEvent, Url},
    Runtime, WindowEvent, Wry,
};

type SharedBackend = Arc<Mutex<Option<BackendProcess>>>;
type SharedOrigin = Arc<Mutex<Option<Url>>>;

#[tauri::command]
fn close_wallet(window: tauri::WebviewWindow) {
    let _ = window.close();
}

pub fn run() {
    let backend: SharedBackend = Arc::new(Mutex::new(None));
    let core_origin: SharedOrigin = Arc::new(Mutex::new(None));
    let started = Arc::new(AtomicBool::new(false));

    let load_backend = Arc::clone(&backend);
    let load_origin = Arc::clone(&core_origin);
    let load_started = Arc::clone(&started);
    let close_backend = Arc::clone(&backend);

    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![close_wallet])
        .plugin(navigation_policy(Arc::clone(&core_origin)))
        .on_page_load(move |webview, payload| {
            if payload.event() != PageLoadEvent::Finished
                || load_started.swap(true, Ordering::AcqRel)
            {
                return;
            }
            let webview = webview.clone();
            let backend = Arc::clone(&load_backend);
            let core_origin = Arc::clone(&load_origin);
            thread::spawn(move || {
                let result = start_bundled_core();
                match result {
                    Ok((process, launch)) => {
                        if let Ok(mut origin) = core_origin.lock() {
                            *origin = Some(launch.url.clone());
                        }
                        if let Ok(mut current) = backend.lock() {
                            *current = Some(process);
                        }
                        if let Err(error) = webview.navigate(launch.url) {
                            show_start_error(
                                &webview,
                                &format!("The wallet window could not open: {error}"),
                            );
                            stop_backend(&backend);
                        }
                    }
                    Err(message) => show_start_error(&webview, &message),
                }
            });
        })
        .on_window_event(move |window, event| {
            if window.label() == "main" && matches!(event, WindowEvent::CloseRequested { .. }) {
                stop_backend(&close_backend);
            }
        })
        .run(tauri::generate_context!())
        .expect("BTC09 Wallet could not start its native window");
}

fn start_bundled_core() -> Result<(BackendProcess, crate::HostedLaunch), String> {
    let executable = std::env::current_exe()
        .map_err(|error| format!("The BTC09 Wallet installation could not be found: {error}"))?;
    let core =
        sidecar_path(&executable, std::env::consts::OS).map_err(|error| error.to_string())?;
    if !core.is_file() {
        return Err("BTC09 Core is missing from this installation. Reinstall BTC09 Wallet.".into());
    }
    let mut command = Command::new(core);
    let test_data_dir = std::env::var_os("BTC09_DESKTOP_DATA_DIR").map(PathBuf::from);
    command.args(desktop_core_arguments(
        test_data_dir.as_deref(),
        cfg!(feature = "wallet-only"),
    ));
    let expected_version = format!("v{}", env!("CARGO_PKG_VERSION"));
    BackendProcess::start(&mut command, &expected_version, Duration::from_secs(15))
        .map_err(|error| error.to_string())
}

fn stop_backend(backend: &SharedBackend) {
    let process = backend.lock().ok().and_then(|mut current| current.take());
    if let Some(mut process) = process {
        let _ = process.stop();
    }
}

fn show_start_error<R: Runtime>(webview: &tauri::Webview<R>, message: &str) {
    let message = serde_json::to_string(message)
        .unwrap_or_else(|_| "\"BTC09 Wallet could not start.\"".to_string());
    let _ = webview.eval(format!("window.showStartError({message})"));
}

fn navigation_policy(origin: SharedOrigin) -> TauriPlugin<Wry> {
    tauri::plugin::Builder::new("navigation-policy")
        .on_navigation(move |_webview, candidate| {
            let core = origin.lock().ok();
            let core = core.as_deref().and_then(|value| value.as_ref());
            navigation_allowed(candidate, core)
        })
        .build()
}
