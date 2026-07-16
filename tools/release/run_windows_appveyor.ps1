[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
Set-Location $repoRoot

& (Join-Path $PSScriptRoot 'install_appveyor_toolchain.ps1')
if (-not $?) {
    throw 'The AppVeyor toolchain setup failed.'
}

& (Join-Path $PSScriptRoot 'build_windows_appveyor.ps1')
if (-not $?) {
    throw 'The AppVeyor Windows build failed.'
}
