[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$VaultPath,

    [Parameter(Mandatory = $true)]
    [string]$CertificateFingerprintPath,

    [string]$OutputDirectory = 'walletapp/src-tauri/target/direct'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$resolvedVault = [System.IO.Path]::GetFullPath($VaultPath)
$resolvedFingerprint = [System.IO.Path]::GetFullPath($CertificateFingerprintPath)
$resolvedOutput = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $OutputDirectory))
$originalAndroidHome = $env:ANDROID_HOME
$originalAndroidSDKRoot = $env:ANDROID_SDK_ROOT

if (-not (Test-Path -LiteralPath $resolvedVault -PathType Leaf)) {
    throw 'The encrypted Android signing vault was not found.'
}
if (-not (Test-Path -LiteralPath $resolvedFingerprint -PathType Leaf)) {
    throw 'The pinned Android certificate fingerprint was not found.'
}

$androidHome = $env:ANDROID_HOME
if ([string]::IsNullOrWhiteSpace($androidHome)) {
    $androidHome = $env:ANDROID_SDK_ROOT
}
if ([string]::IsNullOrWhiteSpace($androidHome)) {
    $androidHome = Join-Path $env:LOCALAPPDATA 'Android\Sdk'
}
$androidHome = [System.IO.Path]::GetFullPath($androidHome)
if (-not (Test-Path -LiteralPath $androidHome -PathType Container)) {
    throw 'The Android SDK was not found.'
}
$env:ANDROID_HOME = $androidHome
$env:ANDROID_SDK_ROOT = $androidHome

$temporaryRoot = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$temporaryPrefix = $temporaryRoot.TrimEnd(
    [System.IO.Path]::DirectorySeparatorChar
) + [System.IO.Path]::DirectorySeparatorChar
$temporaryKey = [System.IO.Path]::GetFullPath(
    (Join-Path $temporaryRoot ("btc09-android-{0}.jks" -f [Guid]::NewGuid().ToString('N')))
)
if (-not $temporaryKey.StartsWith($temporaryPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The temporary Android keystore escaped the operating-system temp directory.'
}

$cipherText = Get-Content -LiteralPath $resolvedVault -Raw
$secureVault = ConvertTo-SecureString $cipherText
$vaultPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureVault)
$plainText = $null
$releaseIdentity = $null

try {
    $plainText = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($vaultPointer)
    $releaseIdentity = $plainText | ConvertFrom-Json
    foreach ($field in @(
        'keystore_base64',
        'store_password',
        'alias',
        'key_password',
        'certificate_sha256'
    )) {
        if ([string]::IsNullOrWhiteSpace([string]$releaseIdentity.$field)) {
            throw "The encrypted Android signing vault is missing $field."
        }
    }

    [System.IO.File]::WriteAllBytes(
        $temporaryKey,
        [Convert]::FromBase64String([string]$releaseIdentity.keystore_base64)
    )

    $env:BTC09_ANDROID_KEYSTORE = $temporaryKey
    $env:BTC09_ANDROID_KEYSTORE_PASSWORD = [string]$releaseIdentity.store_password
    $env:BTC09_ANDROID_KEY_ALIAS = [string]$releaseIdentity.alias
    $env:BTC09_ANDROID_KEY_PASSWORD = [string]$releaseIdentity.key_password

    foreach ($relativeOutput in @(
        'walletapp\src-tauri\gen\android\app\build\outputs\apk\universal\release',
        'walletapp\src-tauri\gen\android\app\build\outputs\bundle\universalRelease'
    )) {
        $staleOutput = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $relativeOutput))
        $repoPrefix = $repoRoot.TrimEnd(
            [System.IO.Path]::DirectorySeparatorChar
        ) + [System.IO.Path]::DirectorySeparatorChar
        if (-not $staleOutput.StartsWith($repoPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'The Android output cleanup path escaped the repository.'
        }
        if (Test-Path -LiteralPath $staleOutput -PathType Container) {
            Remove-Item -LiteralPath $staleOutput -Recurse -Force
        }
    }

    Push-Location $repoRoot
    try {
        & npm --prefix walletapp run mobile:android:build
        if ($LASTEXITCODE -ne 0) {
            throw 'The signed Android release build failed.'
        }
    }
    finally {
        Pop-Location
    }

    $apk = Join-Path $repoRoot 'walletapp\src-tauri\gen\android\app\build\outputs\apk\universal\release\app-universal-release.apk'
    $aab = Join-Path $repoRoot 'walletapp\src-tauri\gen\android\app\build\outputs\bundle\universalRelease\app-universal-release.aab'
    foreach ($artifact in @($apk, $aab)) {
        if (-not (Test-Path -LiteralPath $artifact -PathType Leaf)) {
            throw "The Android release artifact was not produced: $artifact"
        }
    }

    $apksignerCandidate = Get-ChildItem `
        -LiteralPath (Join-Path $env:ANDROID_HOME 'build-tools') `
        -Directory |
        ForEach-Object {
            $apksignerJar = Join-Path $_.FullName 'lib\apksigner.jar'
            if (Test-Path -LiteralPath $apksignerJar -PathType Leaf) {
                $versionMatch = [regex]::Match($_.Name, '^\d+(?:\.\d+){1,3}')
                if ($versionMatch.Success) {
                    [pscustomobject]@{
                        JarPath  = $apksignerJar
                        Version  = [version]$versionMatch.Value
                        IsStable = [regex]::IsMatch($_.Name, '^\d+(?:\.\d+){1,3}$')
                    }
                }
            }
        } |
        Sort-Object Version, IsStable -Descending |
        Select-Object -First 1
    if ($null -eq $apksignerCandidate) {
        throw 'The Android APK signer is unavailable for certificate verification.'
    }
    $apksignerJar = $apksignerCandidate.JarPath
    $javaExecutable = $null
    if (-not [string]::IsNullOrWhiteSpace($env:JAVA_HOME)) {
        $configuredJava = Join-Path $env:JAVA_HOME 'bin\java.exe'
        if (Test-Path -LiteralPath $configuredJava -PathType Leaf) {
            $javaExecutable = $configuredJava
        }
    }
    if ($null -eq $javaExecutable) {
        $javaCommand = Get-Command java -ErrorAction SilentlyContinue
        if ($null -ne $javaCommand) {
            $javaExecutable = $javaCommand.Source
        }
    }
    if ([string]::IsNullOrWhiteSpace($javaExecutable)) {
        throw 'Java is unavailable for Android certificate verification.'
    }

    $signerOutput = & $javaExecutable -jar $apksignerJar `
        verify --verbose --print-certs $apk 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw 'The Android APK signature is invalid.'
    }
    $signerLine = $signerOutput |
        Select-String 'Signer #1 certificate SHA-256 digest:' |
        Select-Object -First 1
    if ($null -eq $signerLine) {
        throw 'The Android APK certificate fingerprint was not reported.'
    }

    $actualFingerprint = (($signerLine.Line -split ':', 2)[1] -replace '[^A-Fa-f0-9]', '').ToUpperInvariant()
    $vaultFingerprint = ([string]$releaseIdentity.certificate_sha256 -replace '[^A-Fa-f0-9]', '').ToUpperInvariant()
    $pinnedFingerprint = (
        (Get-Content -LiteralPath $resolvedFingerprint -Raw) -replace '[^A-Fa-f0-9]', ''
    ).ToUpperInvariant()
    if ($actualFingerprint -ne $vaultFingerprint -or $actualFingerprint -ne $pinnedFingerprint) {
        throw 'The Android APK does not use the preserved BTC09 release certificate.'
    }

    New-Item -ItemType Directory -Path $resolvedOutput -Force | Out-Null
    $publishedApk = Join-Path $resolvedOutput 'btc09-wallet-android-arm64.apk'
    $publishedAab = Join-Path $resolvedOutput 'btc09-wallet-android-arm64.aab'
    Copy-Item -LiteralPath $apk -Destination $publishedApk -Force
    Copy-Item -LiteralPath $aab -Destination $publishedAab -Force

    [pscustomobject]@{
        ApkPath          = $publishedApk
        ApkSize          = (Get-Item -LiteralPath $publishedApk).Length
        ApkSHA256        = (Get-FileHash -LiteralPath $publishedApk -Algorithm SHA256).Hash
        AabPath          = $publishedAab
        AabSize          = (Get-Item -LiteralPath $publishedAab).Length
        AabSHA256        = (Get-FileHash -LiteralPath $publishedAab -Algorithm SHA256).Hash
        CertificateMatch = $true
    }
}
finally {
    Remove-Item Env:BTC09_ANDROID_KEYSTORE -ErrorAction SilentlyContinue
    Remove-Item Env:BTC09_ANDROID_KEYSTORE_PASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:BTC09_ANDROID_KEY_ALIAS -ErrorAction SilentlyContinue
    Remove-Item Env:BTC09_ANDROID_KEY_PASSWORD -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $temporaryKey -PathType Leaf) {
        Remove-Item -LiteralPath $temporaryKey -Force
    }
    if ($null -ne $plainText) {
        $plainText = $null
    }
    if ($null -ne $vaultPointer -and $vaultPointer -ne [IntPtr]::Zero) {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($vaultPointer)
    }
    $env:ANDROID_HOME = $originalAndroidHome
    $env:ANDROID_SDK_ROOT = $originalAndroidSDKRoot
}
