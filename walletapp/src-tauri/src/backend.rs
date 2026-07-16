use crate::{parse_hosted_launch, HostedLaunch};
use std::{
    error::Error,
    ffi::OsString,
    fmt,
    io::{self, BufRead, BufReader},
    path::{Path, PathBuf},
    process::{Child, ChildStdin, Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

pub fn desktop_core_arguments(data_dir: Option<&Path>, wallet_only: bool) -> Vec<OsString> {
    let mut arguments = vec![OsString::from("app"), OsString::from("-desktop-host")];
    if wallet_only {
        arguments.push(OsString::from("-wallet-only"));
    }
    if let Some(data_dir) = data_dir {
        arguments.push(OsString::from("-datadir"));
        arguments.push(data_dir.as_os_str().to_owned());
    }
    arguments
}

#[derive(Debug)]
pub struct BackendError(String);

impl fmt::Display for BackendError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl Error for BackendError {}

#[derive(Debug)]
pub struct BackendProcess {
    child: Child,
    stdin: Option<ChildStdin>,
}

impl BackendProcess {
    pub fn start(
        command: &mut Command,
        expected_version: &str,
        timeout: Duration,
    ) -> Result<(Self, HostedLaunch), BackendError> {
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        hide_child_window(command);

        let mut child = command
            .spawn()
            .map_err(|error| BackendError(format!("BTC09 Core could not start: {error}")))?;
        let stdin = child.stdin.take().ok_or_else(|| {
            BackendError("BTC09 Core did not open its private control pipe.".into())
        })?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| BackendError("BTC09 Core did not open its startup pipe.".into()))?;
        if let Some(stderr) = child.stderr.take() {
            thread::spawn(move || {
                let mut reader = BufReader::new(stderr);
                let _ = io::copy(&mut reader, &mut io::sink());
            });
        }

        let (sender, receiver) = mpsc::sync_channel(1);
        thread::spawn(move || {
            let mut reader = BufReader::new(stdout);
            let mut line = String::new();
            let result = match reader.read_line(&mut line) {
                Ok(0) => Err("BTC09 Core closed before the wallet was ready.".to_string()),
                Ok(_) => Ok(line),
                Err(error) => Err(format!("BTC09 Core startup could not be read: {error}")),
            };
            let _ = sender.send(result);
            let _ = io::copy(&mut reader, &mut io::sink());
        });

        let line = match receiver.recv_timeout(timeout) {
            Ok(Ok(line)) => line,
            Ok(Err(message)) => {
                terminate_child(&mut child, Some(stdin));
                return Err(BackendError(message));
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                terminate_child(&mut child, Some(stdin));
                return Err(BackendError("BTC09 Core took too long to start.".into()));
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                terminate_child(&mut child, Some(stdin));
                return Err(BackendError(
                    "BTC09 Core startup ended unexpectedly.".into(),
                ));
            }
        };

        let launch = match parse_hosted_launch(line.trim_end(), expected_version) {
            Ok(launch) => launch,
            Err(error) => {
                terminate_child(&mut child, Some(stdin));
                return Err(BackendError(error.to_string()));
            }
        };
        Ok((
            Self {
                child,
                stdin: Some(stdin),
            },
            launch,
        ))
    }

    pub fn stop(&mut self) -> Result<(), BackendError> {
        self.stdin.take();
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            match self.child.try_wait() {
                Ok(Some(_)) => return Ok(()),
                Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(20)),
                Ok(None) => {
                    self.child.kill().map_err(|error| {
                        BackendError(format!("BTC09 Core could not stop: {error}"))
                    })?;
                    self.child.wait().map_err(|error| {
                        BackendError(format!("BTC09 Core shutdown could not finish: {error}"))
                    })?;
                    return Err(BackendError(
                        "BTC09 Core did not shut down cleanly and had to be stopped.".into(),
                    ));
                }
                Err(error) => {
                    return Err(BackendError(format!(
                        "BTC09 Core status could not be checked: {error}"
                    )))
                }
            }
        }
    }

    pub fn has_exited(&mut self) -> Result<bool, BackendError> {
        self.child
            .try_wait()
            .map(|status| status.is_some())
            .map_err(|error| {
                BackendError(format!("BTC09 Core status could not be checked: {error}"))
            })
    }
}

impl Drop for BackendProcess {
    fn drop(&mut self) {
        self.stdin.take();
        if matches!(self.child.try_wait(), Ok(None)) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

pub fn sidecar_path(current_executable: &Path, platform: &str) -> Result<PathBuf, BackendError> {
    let source = current_executable.to_string_lossy();
    let (separator, core_name, absolute) = if platform == "windows" {
        let absolute = (source.len() >= 3
            && source.as_bytes()[1] == b':'
            && matches!(source.as_bytes()[2], b'\\' | b'/'))
            || source.starts_with(r"\\");
        ('\\', "btc09-core.exe", absolute)
    } else {
        ('/', "btc09-core", source.starts_with('/'))
    };
    if !absolute {
        return Err(BackendError(
            "The BTC09 Wallet installation path is not absolute.".into(),
        ));
    }
    let index = source
        .rfind(['\\', '/'])
        .ok_or_else(|| BackendError("The BTC09 Wallet installation path is incomplete.".into()))?;
    let mut result = source[..=index].to_string();
    if separator == '/' {
        result = result.replace('\\', "/");
    }
    result.push_str(core_name);
    Ok(PathBuf::from(result))
}

fn terminate_child(child: &mut Child, stdin: Option<ChildStdin>) {
    drop(stdin);
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(windows)]
fn hide_child_window(command: &mut Command) {
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(windows))]
fn hide_child_window(_command: &mut Command) {}
