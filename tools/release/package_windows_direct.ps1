[CmdletBinding()]
param(
    [string]$OutputPath = 'walletapp/src-tauri/target/direct/btc09-wallet-windows-x64-setup.exe',
    [switch]$SkipBuild,
    [switch]$AllowUnsignedPreflight
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$walletRoot = Join-Path $repoRoot 'walletapp'
$targetRoot = [System.IO.Path]::GetFullPath((Join-Path $walletRoot 'src-tauri\target'))
$releaseRoot = Join-Path $targetRoot 'release'
. (Join-Path $PSScriptRoot 'invoke_process_capture.ps1')

function Assert-InTargetRoot([string]$Path) {
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $prefix = $targetRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Direct package paths must stay inside $targetRoot"
    }
    return $fullPath
}

if (-not $SkipBuild) {
    Push-Location $walletRoot
    try {
        & npm run store:build -- --bundles nsis
        if ($LASTEXITCODE -ne 0) {
            throw 'The wallet-only Windows installer build failed.'
        }
    }
    finally {
        Pop-Location
    }
}

$coreExecutable = Join-Path $releaseRoot 'btc09-core.exe'
if (-not (Test-Path -LiteralPath $coreExecutable -PathType Leaf)) {
    throw "Missing wallet-only core: $coreExecutable"
}
$editionProbe = Invoke-CapturedProcess -FilePath $coreExecutable -ArgumentList @('version')
if ($editionProbe.ExitCode -ne 0 -or $editionProbe.Output -notmatch 'wallet edition') {
    throw 'The direct Windows package requires the compile-time wallet edition of BTC09 Core.'
}
$blockedProbe = Invoke-CapturedProcess -FilePath $coreExecutable -ArgumentList @('mine-pool')
if ($blockedProbe.ExitCode -ne 2 -or $blockedProbe.Output -notmatch 'not available in the BTC09 Wallet edition') {
    throw 'The wallet-only core still accepts mining commands.'
}
& node (Join-Path $repoRoot 'tools\desktop\verify-wallet-edition.mjs') $coreExecutable
if ($LASTEXITCODE -ne 0) {
    throw 'The wallet-only core contains mining or demo content.'
}

$nsisRoot = Join-Path $releaseRoot 'bundle\nsis'
$installer = Get-ChildItem -LiteralPath $nsisRoot -Filter '*setup.exe' -File -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTimeUtc -Descending |
    Select-Object -First 1
if ($null -eq $installer) {
    throw "No NSIS installer was found in $nsisRoot"
}

$signature = Get-AuthenticodeSignature -LiteralPath $installer.FullName
if (-not $AllowUnsignedPreflight -and $signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "The Windows installer signature is not trusted ($($signature.Status)). Use -AllowUnsignedPreflight only when preparing a file for the signing service."
}

$resolvedOutput = if ([System.IO.Path]::IsPathRooted($OutputPath)) {
    Assert-InTargetRoot $OutputPath
} else {
    Assert-InTargetRoot (Join-Path $repoRoot $OutputPath)
}
$outputDirectory = Split-Path -Parent $resolvedOutput
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
if (Test-Path -LiteralPath $resolvedOutput) {
    Remove-Item -LiteralPath $resolvedOutput -Force
}
Copy-Item -LiteralPath $installer.FullName -Destination $resolvedOutput

$package = Get-Item -LiteralPath $resolvedOutput
$hash = Get-FileHash -LiteralPath $resolvedOutput -Algorithm SHA256
$signer = if ($null -ne $signature.SignerCertificate) { $signature.SignerCertificate.Subject } else { $null }
[pscustomobject]@{
    Path = $package.FullName
    Size = $package.Length
    SHA256 = $hash.Hash
    SignatureStatus = $signature.Status
    Signer = $signer
    Preflight = [bool]$AllowUnsignedPreflight
} | Format-List
