[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
Set-Location $repoRoot

& go test ./...
if ($LASTEXITCODE -ne 0) {
    throw 'The Go test suite failed.'
}
& go test -tags walletedition ./desktop ./cmd/btc09
if ($LASTEXITCODE -ne 0) {
    throw 'The wallet-only Go tests failed.'
}

$desktopTests = @(
    Get-ChildItem -LiteralPath (Join-Path $repoRoot 'tools\desktop') -File |
        Where-Object { $_.Name -match '\.test\.(mjs|cjs)$' } |
        Sort-Object Name |
        Select-Object -ExpandProperty FullName
)
& node --test @desktopTests
if ($LASTEXITCODE -ne 0) {
    throw 'The native wallet contract tests failed.'
}

Push-Location (Join-Path $repoRoot 'walletapp')
try {
    & npm ci
    if ($LASTEXITCODE -ne 0) {
        throw 'The native wallet dependencies could not be installed.'
    }
}
finally {
    Pop-Location
}

& node tools/desktop/prepare-sidecar.mjs wallet
if ($LASTEXITCODE -ne 0) {
    throw 'The wallet-only Windows sidecar could not be prepared for Tauri tests.'
}

& cargo fmt --manifest-path walletapp/src-tauri/Cargo.toml -- --check
if ($LASTEXITCODE -ne 0) {
    throw 'The native wallet Rust formatting check failed.'
}
& cargo test --manifest-path walletapp/src-tauri/Cargo.toml
if ($LASTEXITCODE -ne 0) {
    throw 'The native wallet Rust tests failed.'
}

& (Join-Path $repoRoot 'tools\release\package_windows_direct.ps1') -AllowUnsignedPreflight
if ($LASTEXITCODE -ne 0) {
    throw 'The Windows wallet preflight package failed.'
}

$core = Join-Path $repoRoot 'walletapp\src-tauri\target\release\btc09-core.exe'
& node tools/desktop/verify-wallet-edition.mjs $core
if ($LASTEXITCODE -ne 0) {
    throw 'The packaged sidecar is not the wallet-only edition.'
}

$installer = Join-Path $repoRoot 'walletapp\src-tauri\target\direct\btc09-wallet-windows-x64-setup.exe'
if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) {
    throw 'The expected Windows installer was not produced.'
}
$item = Get-Item -LiteralPath $installer
if ($item.VersionInfo.ProductName -ne 'BTC09 Wallet' -or
    $item.VersionInfo.ProductVersion -ne '0.1.33' -or
    $item.VersionInfo.FileVersion -ne '0.1.33') {
    throw 'The Windows installer metadata is not safe for the signing policy.'
}
$signature = Get-AuthenticodeSignature -LiteralPath $installer
if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::NotSigned) {
    throw "The trusted build must hand SignPath an unsigned installer, not $($signature.Status)."
}

$hash = Get-FileHash -LiteralPath $installer -Algorithm SHA256
Write-Host "APPVEYOR_WINDOWS_ARTIFACT=$($item.FullName)"
Write-Host "APPVEYOR_WINDOWS_SHA256=$($hash.Hash.ToLowerInvariant())"
