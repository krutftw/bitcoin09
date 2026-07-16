mod backend;
#[cfg(desktop)]
mod native;

pub use backend::{desktop_core_arguments, sidecar_path, BackendError, BackendProcess};

use serde::Deserialize;
use std::{error::Error, fmt};
use url::{Host, Url};

#[derive(Debug)]
pub struct HostedLaunch {
    pub version: String,
    pub url: Url,
}

#[derive(Debug)]
pub struct LaunchError(String);

impl fmt::Display for LaunchError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl Error for LaunchError {}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct HostedLaunchWire {
    schema_version: u32,
    version: String,
    launch_url: String,
}

pub fn parse_hosted_launch(
    line: &str,
    expected_version: &str,
) -> Result<HostedLaunch, LaunchError> {
    let launch: HostedLaunchWire = serde_json::from_str(line)
        .map_err(|_| LaunchError("BTC09 Core returned unreadable startup information.".into()))?;
    if launch.schema_version != 1 {
        return Err(LaunchError(
            "BTC09 Core uses an unsupported startup format.".into(),
        ));
    }
    if launch.version != expected_version {
        return Err(LaunchError(format!(
            "BTC09 Core version {} does not match wallet version {}.",
            launch.version, expected_version
        )));
    }

    let url = Url::parse(&launch.launch_url)
        .map_err(|_| LaunchError("BTC09 Core returned an invalid local wallet link.".into()))?;
    let loopback = matches!(url.host(), Some(Host::Ipv4(address)) if address.is_loopback())
        || matches!(url.host(), Some(Host::Ipv6(address)) if address.is_loopback());
    let has_safe_origin = url.scheme() == "http"
        && loopback
        && url.port().is_some()
        && url.username().is_empty()
        && url.password().is_none();
    let token = url.query().and_then(|query| query.strip_prefix("token="));
    let has_safe_token = token.is_some_and(|value| {
        value.len() == 64
            && value.bytes().all(|byte| byte.is_ascii_hexdigit())
            && !value.contains('&')
    });
    if !has_safe_origin || url.path() != "/" || url.fragment().is_some() || !has_safe_token {
        return Err(LaunchError(
            "BTC09 Core returned an unsafe local wallet link.".into(),
        ));
    }

    Ok(HostedLaunch {
        version: launch.version,
        url,
    })
}

pub fn navigation_allowed(candidate: &Url, core_origin: Option<&Url>) -> bool {
    let packaged_origin = (candidate.scheme() == "tauri"
        && candidate.host_str() == Some("localhost"))
        || (matches!(candidate.scheme(), "http" | "https")
            && candidate.host_str() == Some("tauri.localhost"));
    let bundled_ui = packaged_origin
        && candidate.port().is_none()
        && candidate.username().is_empty()
        && candidate.password().is_none();
    if bundled_ui {
        return true;
    }
    core_origin.is_some_and(|core| {
        candidate.scheme() == "http"
            && candidate.scheme() == core.scheme()
            && candidate.host_str() == core.host_str()
            && candidate.port() == core.port()
    })
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    #[cfg(desktop)]
    native::run();

    #[cfg(mobile)]
    tauri::Builder::default()
        .plugin(tauri_plugin_wallet_core::init())
        .run(tauri::generate_context!())
        .expect("BTC09 Wallet could not start");
}

#[cfg(test)]
mod tests {
    use super::{
        desktop_core_arguments, navigation_allowed, parse_hosted_launch, sidecar_path,
        BackendProcess,
    };
    use std::{path::Path, process::Command, time::Duration};

    const VERSION: &str = "v0.1.33";
    const TOKEN: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn launch_json(version: &str, url: &str) -> String {
        serde_json::json!({
            "schema_version": 1,
            "version": version,
            "launch_url": url,
        })
        .to_string()
    }

    #[test]
    fn accepts_the_matching_core_on_a_private_loopback_link() {
        let line = launch_json(VERSION, &format!("http://127.0.0.1:49152/?token={TOKEN}"));
        let launch = parse_hosted_launch(&line, VERSION).expect("valid desktop launch");

        assert_eq!(launch.version, VERSION);
        assert_eq!(
            launch.url.as_str(),
            format!("http://127.0.0.1:49152/?token={TOKEN}")
        );
    }

    #[test]
    fn rejects_a_mismatched_or_malformed_core() {
        let wrong_version =
            launch_json("v0.1.32", &format!("http://127.0.0.1:49152/?token={TOKEN}"));
        assert!(parse_hosted_launch(&wrong_version, VERSION)
            .unwrap_err()
            .to_string()
            .contains("version"));

        for line in [
            "not json".to_string(),
            serde_json::json!({
                "schema_version": 2,
                "version": VERSION,
                "launch_url": format!("http://127.0.0.1:49152/?token={TOKEN}"),
            })
            .to_string(),
        ] {
            assert!(
                parse_hosted_launch(&line, VERSION).is_err(),
                "accepted {line}"
            );
        }
    }

    #[test]
    fn rejects_links_that_could_escape_the_local_wallet() {
        for candidate in [
            format!("https://127.0.0.1:49152/?token={TOKEN}"),
            format!("http://localhost:49152/?token={TOKEN}"),
            format!("http://example.com:49152/?token={TOKEN}"),
            format!("http://127.0.0.1:49152/path?token={TOKEN}"),
            "http://127.0.0.1:49152/?token=short".to_string(),
            format!("http://127.0.0.1:49152/?token={TOKEN}&next=https://example.com"),
            format!("http://127.0.0.1:49152/?token={TOKEN}#outside"),
        ] {
            let line = launch_json(VERSION, &candidate);
            assert!(
                parse_hosted_launch(&line, VERSION).is_err(),
                "accepted {candidate}"
            );
        }
    }

    #[test]
    fn resolves_the_bundled_core_beside_the_native_app() {
        assert_eq!(
            sidecar_path(
                Path::new(r"C:\Program Files\BTC09 Wallet\BTC09 Wallet.exe"),
                "windows"
            )
            .expect("Windows sidecar path"),
            Path::new(r"C:\Program Files\BTC09 Wallet\btc09-core.exe")
        );
        assert_eq!(
            sidecar_path(
                Path::new("/Applications/BTC09 Wallet.app/Contents/MacOS/BTC09 Wallet"),
                "macos"
            )
            .expect("macOS sidecar path"),
            Path::new("/Applications/BTC09 Wallet.app/Contents/MacOS/btc09-core")
        );
        assert!(sidecar_path(Path::new("BTC09 Wallet.exe"), "windows").is_err());
    }

    #[test]
    fn isolated_desktop_tests_can_use_a_separate_data_directory() {
        let arguments =
            desktop_core_arguments(Some(Path::new(r"C:\Temp\btc09-native-test")), false);
        assert_eq!(arguments[0], "app");
        assert_eq!(arguments[1], "-desktop-host");
        assert_eq!(arguments[2], "-datadir");
        assert_eq!(arguments[3], r"C:\Temp\btc09-native-test");
        assert_eq!(desktop_core_arguments(None, false).len(), 2);
    }

    #[test]
    fn wallet_only_build_disables_on_device_mining_in_the_core() {
        let arguments = desktop_core_arguments(None, true);
        assert_eq!(arguments.len(), 3);
        assert_eq!(arguments[2], "-wallet-only");
    }

    #[test]
    fn native_window_stays_on_the_bundled_ui_and_its_exact_core_origin() {
        let core =
            url::Url::parse(&format!("http://127.0.0.1:49152/?token={TOKEN}")).expect("core URL");
        for allowed in [
            "tauri://localhost/index.html",
            "http://tauri.localhost/",
            "http://127.0.0.1:49152/app",
            "http://127.0.0.1:49152/assets/app.js",
        ] {
            let url = url::Url::parse(allowed).expect("allowed URL");
            assert!(navigation_allowed(&url, Some(&core)), "blocked {allowed}");
        }
        for blocked in [
            "https://btc09.org/",
            "http://127.0.0.1:49153/",
            "http://localhost:49152/",
            "tauri://outside/index.html",
            "https://tauri.localhost:444/index.html",
            "http://user@tauri.localhost/index.html",
            "file:///C:/Windows/System32/drivers/etc/hosts",
        ] {
            let url = url::Url::parse(blocked).expect("blocked URL");
            assert!(!navigation_allowed(&url, Some(&core)), "allowed {blocked}");
        }
    }

    #[test]
    fn closes_the_core_cleanly_when_the_native_window_closes() {
        let line = launch_json(VERSION, &format!("http://127.0.0.1:49152/?token={TOKEN}"));
        let mut command = helper_command(&line);
        let (mut process, launch) =
            BackendProcess::start(&mut command, VERSION, Duration::from_secs(3))
                .expect("start mock BTC09 Core");
        assert_eq!(launch.version, VERSION);
        process.stop().expect("stop mock BTC09 Core");
        assert!(process.has_exited().expect("mock process status"));
    }

    #[test]
    fn rejects_a_core_that_does_not_match_the_native_app() {
        let line = launch_json("v0.1.32", &format!("http://127.0.0.1:49152/?token={TOKEN}"));
        let mut command = helper_command(&line);
        let error = BackendProcess::start(&mut command, VERSION, Duration::from_secs(3))
            .expect_err("mismatched core was accepted");
        assert!(error.to_string().contains("version"));
    }

    #[cfg(windows)]
    fn helper_command(line: &str) -> Command {
        let escaped = line.replace(char::from(39), "''");
        let mut command = Command::new("powershell.exe");
        command.args([
            "-NoLogo",
            "-NoProfile",
            "-NonInteractive",
            "-Command",
            &format!(
                "[Console]::Out.WriteLine('{}'); [Console]::Out.Flush(); [Console]::In.ReadToEnd() | Out-Null",
                escaped
            ),
        ]);
        command
    }

    #[cfg(not(windows))]
    fn helper_command(line: &str) -> Command {
        let escaped = line.replace(char::from(39), "'\\''");
        let mut command = Command::new("sh");
        command.args(["-c", &format!("printf '%s\\n' '{escaped}'; cat >/dev/null")]);
        command
    }
}
