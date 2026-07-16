[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
Set-Location $repoRoot

# AppVeyor supplies CI=True, but Tauri parses CI as a strict lowercase boolean.
$env:CI = 'true'

& (Join-Path $PSScriptRoot 'install_appveyor_toolchain.ps1')
if (-not $?) {
    throw 'The AppVeyor toolchain setup failed.'
}

# Rust keeps source paths for panic diagnostics even in release binaries. Use
# stable virtual roots so the signed wallet never exposes a CI account path.
$cargoHome = [System.IO.Path]::GetFullPath((Join-Path $env:USERPROFILE '.cargo'))
$rustupHome = [System.IO.Path]::GetFullPath((Join-Path $env:USERPROFILE '.rustup'))
$rustRemapFlags = @(
    "--remap-path-prefix=$repoRoot=/src/bitcoin09"
    "--remap-path-prefix=$cargoHome=/cargo"
    "--remap-path-prefix=$rustupHome=/rustup"
)
$env:CARGO_ENCODED_RUSTFLAGS = $rustRemapFlags -join [char]0x1f

& (Join-Path $PSScriptRoot 'build_windows_appveyor.ps1')
if (-not $?) {
    throw 'The AppVeyor Windows build failed.'
}
