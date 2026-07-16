[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Za-z0-9.-]{3,50}$')]
    [string]$IdentityName,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^CN=.+')]
    [string]$Publisher,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$PublisherDisplayName,

    [string]$OutputPath = 'walletapp/src-tauri/target/store/btc09-wallet-store.msix',

    [switch]$SkipBuild
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$walletRoot = Join-Path $repoRoot 'walletapp'
$targetRoot = [System.IO.Path]::GetFullPath((Join-Path $walletRoot 'src-tauri\target'))
$releaseRoot = Join-Path $targetRoot 'release'

function Assert-InTargetRoot([string]$Path) {
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $prefix = $targetRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Store package paths must stay inside $targetRoot"
    }
    return $fullPath
}

function Remove-TargetPath([string]$Path) {
    $fullPath = Assert-InTargetRoot $Path
    if (Test-Path -LiteralPath $fullPath) {
        Remove-Item -LiteralPath $fullPath -Recurse -Force
    }
}

function Find-MakeAppx {
    $programFilesX86 = ${env:ProgramFiles(x86)}
    if ([string]::IsNullOrWhiteSpace($programFilesX86)) {
        throw 'The Windows SDK could not be located.'
    }
    $sdkBin = Join-Path $programFilesX86 'Windows Kits\10\bin'
    $versions = Get-ChildItem -LiteralPath $sdkBin -Directory -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match '^\d+\.\d+\.\d+\.\d+$' } |
        Sort-Object { [version]$_.Name } -Descending
    foreach ($version in $versions) {
        $candidate = Join-Path $version.FullName 'x64\MakeAppx.exe'
        if (Test-Path -LiteralPath $candidate) {
            return $candidate
        }
    }
    throw 'MakeAppx.exe is missing. Install the Windows SDK before packaging the Store build.'
}

function Get-PeMachine([string]$Path) {
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    try {
        if ($stream.Length -lt 64) {
            throw "$Path is too small to be a Windows executable."
        }
        $reader = [System.IO.BinaryReader]::new($stream)
        try {
            if ($reader.ReadUInt16() -ne 0x5A4D) {
                throw "$Path does not have a valid DOS executable header."
            }
            $stream.Position = 0x3C
            $peOffset = $reader.ReadUInt32()
            if ($peOffset -gt ($stream.Length - 6)) {
                throw "$Path has an invalid PE header offset."
            }
            $stream.Position = $peOffset
            if ($reader.ReadUInt32() -ne 0x00004550) {
                throw "$Path does not have a valid PE signature."
            }
            return $reader.ReadUInt16()
        }
        finally {
            $reader.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

function Assert-X64Pe([string]$Path) {
    $machine = Get-PeMachine $Path
    if ($machine -ne 0x8664) {
        throw ("{0} targets PE machine 0x{1:X4}; the x64 Store package requires 0x8664." -f $Path, $machine)
    }
}

if (-not $SkipBuild) {
    Push-Location $walletRoot
    try {
        # This exact command compiles the shell with the Rust wallet-only feature.
        & npm run store:build -- --no-bundle
        if ($LASTEXITCODE -ne 0) {
            throw 'The wallet-only Windows build failed.'
        }
    }
    finally {
        Pop-Location
    }
}

$cargoPath = Join-Path $walletRoot 'src-tauri\Cargo.toml'
$cargo = Get-Content -LiteralPath $cargoPath -Raw
$versionMatch = [regex]::Match($cargo, '(?m)^version\s*=\s*"0\.(\d+)\.(\d+)"\s*$')
if (-not $versionMatch.Success) {
    throw 'Version must use 0.minor.patch before it can be mapped to a Microsoft Store version.'
}
$minor = [int]$versionMatch.Groups[1].Value
$patch = [int]$versionMatch.Groups[2].Value
if ($minor -lt 1 -or $minor -gt 65535 -or $patch -gt 65535) {
    throw 'The wallet version does not fit the four-part Microsoft Store version format.'
}
# Example: BTC09 Wallet 0.1.34 maps monotonically to Store version 1.34.0.0.
$storeVersion = "$minor.$patch.0.0"

$stageRoot = Join-Path $targetRoot 'store-msix\stage'
$verifyRoot = Join-Path $targetRoot 'store-msix\verify'
$resolvedOutput = if ([System.IO.Path]::IsPathRooted($OutputPath)) {
    Assert-InTargetRoot $OutputPath
} else {
    Assert-InTargetRoot (Join-Path $repoRoot $OutputPath)
}
Remove-TargetPath $stageRoot
Remove-TargetPath $verifyRoot
Remove-TargetPath $resolvedOutput
New-Item -ItemType Directory -Path $stageRoot | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stageRoot 'Assets') | Out-Null
New-Item -ItemType Directory -Path (Split-Path -Parent $resolvedOutput) -Force | Out-Null

$walletExecutable = Join-Path $releaseRoot 'btc09-wallet.exe'
$coreExecutable = Join-Path $releaseRoot 'btc09-core.exe'
foreach ($required in ($walletExecutable, $coreExecutable)) {
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) {
        throw "Missing wallet-only build output: $required"
    }
    Assert-X64Pe $required
}
$editionOutput = (& $coreExecutable version 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $editionOutput -notmatch 'wallet edition') {
    throw 'The Microsoft Store package requires the compile-time wallet edition of BTC09 Core.'
}
$blockedOutput = (& $coreExecutable mine-pool 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 2 -or $blockedOutput -notmatch 'not available in the BTC09 Wallet edition') {
    throw 'The Microsoft Store BTC09 Core still accepts non-wallet commands.'
}
& node (Join-Path $repoRoot 'tools\desktop\verify-wallet-edition.mjs') $coreExecutable
if ($LASTEXITCODE -ne 0) {
    throw 'The Microsoft Store BTC09 Core contains code or interface content outside the wallet edition.'
}
Copy-Item -LiteralPath $walletExecutable -Destination (Join-Path $stageRoot 'btc09-wallet.exe')
Copy-Item -LiteralPath $coreExecutable -Destination (Join-Path $stageRoot 'btc09-core.exe')

$iconsRoot = Join-Path $walletRoot 'src-tauri\icons'
foreach ($icon in ('StoreLogo.png', 'Square44x44Logo.png', 'Square150x150Logo.png')) {
    Copy-Item -LiteralPath (Join-Path $iconsRoot $icon) -Destination (Join-Path $stageRoot "Assets\$icon")
}

$escapedIdentity = [System.Security.SecurityElement]::Escape($IdentityName)
$escapedPublisher = [System.Security.SecurityElement]::Escape($Publisher)
$escapedPublisherName = [System.Security.SecurityElement]::Escape($PublisherDisplayName)
$manifest = @"
<?xml version="1.0" encoding="utf-8"?>
<Package
  xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10"
  xmlns:uap="http://schemas.microsoft.com/appx/manifest/uap/windows10"
  xmlns:uap10="http://schemas.microsoft.com/appx/manifest/uap/windows10/10"
  xmlns:rescap="http://schemas.microsoft.com/appx/manifest/foundation/windows10/restrictedcapabilities"
  IgnorableNamespaces="uap uap10 rescap">
  <Identity Name="$escapedIdentity" Publisher="$escapedPublisher" Version="$storeVersion" ProcessorArchitecture="x64" />
  <Properties>
    <DisplayName>BTC09 Wallet</DisplayName>
    <PublisherDisplayName>$escapedPublisherName</PublisherDisplayName>
    <Description>Send, receive and manage Bitcoin 09 in a self-custody wallet.</Description>
    <Logo>Assets\StoreLogo.png</Logo>
  </Properties>
  <Resources>
    <Resource Language="en-us" />
  </Resources>
  <Dependencies>
    <TargetDeviceFamily Name="Windows.Desktop" MinVersion="10.0.19041.0" MaxVersionTested="10.0.26100.0" />
  </Dependencies>
  <Capabilities>
    <rescap:Capability Name="runFullTrust" />
  </Capabilities>
  <Applications>
    <Application Id="BTC09Wallet" Executable="btc09-wallet.exe" uap10:RuntimeBehavior="packagedClassicApp" uap10:TrustLevel="mediumIL">
      <uap:VisualElements
        DisplayName="BTC09 Wallet"
        Description="Send, receive and manage Bitcoin 09."
        BackgroundColor="#F5F1E8"
        Square150x150Logo="Assets\Square150x150Logo.png"
        Square44x44Logo="Assets\Square44x44Logo.png" />
    </Application>
  </Applications>
</Package>
"@
Set-Content -LiteralPath (Join-Path $stageRoot 'AppxManifest.xml') -Value $manifest -Encoding utf8NoBOM

$makeAppx = Find-MakeAppx
# MakeAppx pack validates the manifest while creating an unsigned Store submission package.
& $makeAppx pack /o /h SHA256 /d $stageRoot /p $resolvedOutput
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $resolvedOutput -PathType Leaf)) {
    throw 'MakeAppx did not produce the Microsoft Store package.'
}

New-Item -ItemType Directory -Path $verifyRoot | Out-Null
& $makeAppx unpack /o /p $resolvedOutput /d $verifyRoot
if ($LASTEXITCODE -ne 0) {
    throw 'The Microsoft Store package could not be unpacked for verification.'
}
foreach ($required in ('AppxManifest.xml', 'btc09-wallet.exe', 'btc09-core.exe', 'Assets\StoreLogo.png')) {
    if (-not (Test-Path -LiteralPath (Join-Path $verifyRoot $required) -PathType Leaf)) {
        throw "The Microsoft Store package is missing $required"
    }
}
Assert-X64Pe (Join-Path $verifyRoot 'btc09-wallet.exe')
Assert-X64Pe (Join-Path $verifyRoot 'btc09-core.exe')
if (Test-Path -LiteralPath (Join-Path $verifyRoot 'AppxSignature.p7x')) {
    throw 'The preflight package unexpectedly contains a signature.'
}

$package = Get-Item -LiteralPath $resolvedOutput
$hash = Get-FileHash -LiteralPath $resolvedOutput -Algorithm SHA256
[pscustomobject]@{
    Path = $package.FullName
    Version = $storeVersion
    Size = $package.Length
    SHA256 = $hash.Hash
    Signed = $false
} | Format-List
