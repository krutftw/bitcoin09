[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$goVersion = '1.25.12'
$goArchiveName = 'go1.25.12.windows-amd64.zip'
$goArchiveSHA256 = 'd5dc82da351b00e5eedd04f41356817d674cc4308131f0f638a5b14c5c3af4cb'
$rustVersion = '1.95.0'
$rustupSHA256 = '86478e53f769379d7f0ebfa7c9aa97cb76ca92233f79aa2cc0dbee2efaac73c7'

$requestedGoVersion = [Environment]::GetEnvironmentVariable('BTC09_GO_VERSION')
$requestedRustVersion = [Environment]::GetEnvironmentVariable('BTC09_RUST_VERSION')
if ($requestedGoVersion -and $requestedGoVersion -ne $goVersion) {
    throw "AppVeyor requested Go $requestedGoVersion, but this build is pinned to $goVersion."
}
if ($requestedRustVersion -and $requestedRustVersion -ne $rustVersion) {
    throw "AppVeyor requested Rust $requestedRustVersion, but this build is pinned to $rustVersion."
}

$toolsRoot = [System.IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'btc09-build-tools'))
$allowedRoot = [System.IO.Path]::GetFullPath($env:LOCALAPPDATA).TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
if (-not $toolsRoot.StartsWith($allowedRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw 'The AppVeyor tool directory escaped the local application-data folder.'
}
New-Item -ItemType Directory -Path $toolsRoot -Force | Out-Null

$goArchive = Join-Path $toolsRoot $goArchiveName
$goRoot = Join-Path $toolsRoot 'go'
Invoke-WebRequest -Uri "https://go.dev/dl/$goArchiveName" -OutFile $goArchive
if ((Get-FileHash -LiteralPath $goArchive -Algorithm SHA256).Hash -ne $goArchiveSHA256) {
    throw 'The pinned Go archive checksum did not match.'
}
if (Test-Path -LiteralPath $goRoot) {
    Remove-Item -LiteralPath $goRoot -Recurse -Force
}
Expand-Archive -LiteralPath $goArchive -DestinationPath $toolsRoot -Force
Remove-Item -LiteralPath $goArchive -Force

$cargoBin = Join-Path $env:USERPROFILE '.cargo\bin'
$env:PATH = "$cargoBin;$goRoot\bin;$env:PATH"
$rustup = Get-Command rustup -ErrorAction SilentlyContinue
if ($null -eq $rustup) {
    $rustupInit = Join-Path $toolsRoot 'rustup-init.exe'
    Invoke-WebRequest -Uri 'https://static.rust-lang.org/rustup/dist/x86_64-pc-windows-msvc/rustup-init.exe' -OutFile $rustupInit
    if ((Get-FileHash -LiteralPath $rustupInit -Algorithm SHA256).Hash -ne $rustupSHA256) {
        throw 'The rustup installer checksum did not match.'
    }
    & $rustupInit -y --profile minimal --default-toolchain none
    if ($LASTEXITCODE -ne 0) {
        throw 'rustup installation failed.'
    }
    Remove-Item -LiteralPath $rustupInit -Force
}

& rustup toolchain install 1.95.0 --profile minimal --no-self-update
if ($LASTEXITCODE -ne 0) {
    throw 'The pinned Rust toolchain installation failed.'
}
$env:RUSTUP_TOOLCHAIN = $rustVersion
& rustup component add rustfmt --toolchain $rustVersion
if ($LASTEXITCODE -ne 0) {
    throw 'The Rust formatter component could not be installed.'
}

$env:GOTOOLCHAIN = 'local'
if (Get-Command Set-AppveyorBuildVariable -ErrorAction SilentlyContinue) {
    Set-AppveyorBuildVariable -Name PATH -Value $env:PATH
    Set-AppveyorBuildVariable -Name GOTOOLCHAIN -Value $env:GOTOOLCHAIN
    Set-AppveyorBuildVariable -Name RUSTUP_TOOLCHAIN -Value $env:RUSTUP_TOOLCHAIN
}

if ((& go version) -notmatch "go$([regex]::Escape($goVersion))") {
    throw 'The pinned Go toolchain is not active.'
}
if ((& rustc --version) -notmatch "rustc $([regex]::Escape($rustVersion))") {
    throw 'The pinned Rust toolchain is not active.'
}
if ((& node --version) -notmatch '^v24\.16\.0$') {
    throw 'The pinned Node.js toolchain is not active.'
}
