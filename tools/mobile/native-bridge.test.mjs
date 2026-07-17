import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const plugin = path.join(root, "walletapp", "plugins", "tauri-plugin-wallet-core");

const load = (relative) => readFile(path.join(plugin, relative), "utf8");

test("Android keeps the device key out of the web view and locks in the background", async () => {
  const [bridge, keyStore, gradle, manifest, appManifest, backupRules, extractionRules, appGradle, theme, nightTheme, filePaths, activity] = await Promise.all([
    load("android/src/main/java/WalletCorePlugin.kt"),
    load("android/src/main/java/DeviceKeyStore.kt"),
    load("android/build.gradle.kts"),
    load("android/src/main/AndroidManifest.xml"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "AndroidManifest.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res", "xml", "backup_rules.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res", "xml", "data_extraction_rules.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "build.gradle.kts"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res", "values", "themes.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res", "values-night", "themes.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res", "xml", "file_paths.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "java", "org", "bitcoin09", "wallet", "MainActivity.kt"), "utf8"),
  ]);

  assert.match(bridge, /override fun onPause\(\)[\s\S]*engineInstance\?\.lock\(\)/);
  const pauseBody = bridge.match(/override fun onPause\(\) \{([\s\S]*?)\n    }/)?.[1] || "";
  assert.doesNotMatch(pauseBody, /cancelAuthentication\(\)|authenticationInProgress\.set\(false\)/);
  assert.match(bridge, /Build\.VERSION\.SDK_INT >= Build\.VERSION_CODES\.R/);
  assert.match(bridge, /KeyguardManager/);
  assert.match(bridge, /setDeviceCredentialAllowed\(true\)/);
  assert.match(bridge, /BiometricManager\.Authenticators\.BIOMETRIC_STRONG/);
  assert.match(bridge, /BiometricManager\.Authenticators\.DEVICE_CREDENTIAL/);
  assert.match(bridge, /onAuthenticationError[\s\S]*engineInstance\?\.lock\(\)/);
  assert.match(bridge, /Protect your new BTC09 wallet/);
  assert.match(bridge, /Executors\.newSingleThreadExecutor/);
  assert.match(bridge, /key\.fill\(0\)/);
  assert.doesNotMatch(bridge, /Log\.|println\(|deviceKey.*put\(/);
  assert.match(keyStore, /AndroidKeyStore/);
  assert.match(keyStore, /AES\/GCM\/NoPadding/);
  assert.match(keyStore, /setUnlockedDeviceRequired\(true\)/);
  assert.match(keyStore, /setUserAuthenticationRequired\(true\)/);
  assert.match(keyStore, /setUserAuthenticationParameters/);
  assert.match(keyStore, /KeyProperties\.AUTH_BIOMETRIC_STRONG/);
  assert.match(keyStore, /KeyProperties\.AUTH_DEVICE_CREDENTIAL/);
  assert.match(keyStore, /SecureRandom\(\)\.nextBytes/);
  assert.match(keyStore, /hasCiphertext != hasIV/);
  assert.match(gradle, /libs\/mobilewallet\.aar/);
  assert.match(gradle, /consumerProguardFiles\("consumer-rules\.pro"\)/);
  assert.match(gradle, /minSdk = 24/);
  assert.match(gradle, /compilerOptions[\s\S]*JvmTarget\.JVM_17/);
  assert.doesNotMatch(gradle, /kotlinOptions/);
  assert.match(manifest, /android\.permission\.INTERNET/);
  assert.match(manifest, /android:allowBackup="false"/);
  assert.match(manifest, /android:fullBackupContent="false"/);
  assert.match(manifest, /android:usesCleartextTraffic="false"/);
  assert.match(appManifest, /android:allowBackup="false"/);
  assert.match(appManifest, /android:dataExtractionRules="@xml\/data_extraction_rules"/);
  assert.match(appManifest, /android:fullBackupContent="@xml\/backup_rules"/);
  assert.match(appManifest, /android:usesCleartextTraffic="false"/);
  assert.match(appManifest, /tools:replace="android:allowBackup,android:fullBackupContent,android:usesCleartextTraffic"/);
  assert.doesNotMatch(appManifest, /LEANBACK|usesCleartextTraffic}/);
  for (const rules of [backupRules, extractionRules]) {
    for (const domain of ["root", "file", "database", "sharedpref", "external", "device_root", "device_file", "device_database", "device_sharedpref"]) {
      assert.match(rules, new RegExp(`<exclude domain=["']${domain}["'] path=["']\\.["']`));
    }
  }
  assert.match(extractionRules, /<cloud-backup>/);
  assert.match(extractionRules, /<device-transfer>/);
  assert.match(appGradle, /ndkVersion = "29\.0\.14206865"/);
  assert.match(appGradle, /compileOptions[\s\S]*JavaVersion\.VERSION_17/);
  assert.match(appGradle, /compilerOptions[\s\S]*JvmTarget\.JVM_17/);
  assert.doesNotMatch(appGradle, /kotlinOptions/);
  assert.doesNotMatch(appGradle, /usesCleartextTraffic.*true/);
  for (const value of [theme, nightTheme]) {
    assert.match(value, /Theme\.MaterialComponents\.Light\.NoActionBar/);
    assert.match(value, /#f5f1e8/i);
    assert.doesNotMatch(value, /DayNight|purple_|teal_|3DDC84/);
  }
  assert.match(filePaths, /<cache-path name="shared_cache" path="shared\/"/);
  assert.doesNotMatch(filePaths, /external-path|external-cache-path|path="\."/);
  assert.match(activity, /WindowManager\.LayoutParams\.FLAG_SECURE/);
});

test("iPhone uses a this-device-only user-presence key and locks in the background", async () => {
  const [bridge, keyStore, manifest, tauriConfigText] = await Promise.all([
    load("ios/Sources/WalletCorePlugin.swift"),
    load("ios/Sources/DeviceKeyStore.swift"),
    load("ios/Package.swift"),
    readFile(path.join(root, "walletapp", "src-tauri", "tauri.conf.json"), "utf8"),
  ]);

  assert.match(bridge, /UIApplication\.didEnterBackgroundNotification/);
  assert.match(bridge, /UIApplication\.willResignActiveNotification/);
  assert.match(bridge, /UIApplication\.didBecomeActiveNotification/);
  assert.match(bridge, /privacyOverlay/);
  assert.match(bridge, /UIApplication\.protectedDataWillBecomeUnavailableNotification/);
  assert.match(bridge, /engine\?\.lock\(\)/);
  assert.match(bridge, /DispatchQueue\(label: "org\.bitcoin09\.wallet-core"/);
  assert.match(bridge, /isExcludedFromBackup/);
  assert.match(bridge, /FileProtectionType\.complete/);
  assert.match(bridge, /resetBytes/);
  assert.match(bridge, /ActivityArgs: Decodable \{ let limit: Int \}/);
  assert.match(bridge, /\(MobilewalletEngine, NSErrorPointer\) -> String\?/);
  assert.match(bridge, /try \$0\.cancelSend\(args\.pendingId\)/);
  assert.doesNotMatch(bridge, /UnsafeMutablePointer<NSError\?>/);
  assert.doesNotMatch(bridge, /cancelSend\(args\.pendingId, error:/);
  assert.doesNotMatch(bridge, /print\(|NSLog/);
  assert.match(keyStore, /kSecAttrAccessibleWhenUnlockedThisDeviceOnly/);
  assert.match(keyStore, /\.userPresence/);
  assert.match(keyStore, /kSecAttrSynchronizable as String: false/);
  assert.match(keyStore, /SecRandomCopyBytes/);
  assert.match(keyStore, /errSecDuplicateItem/);
  assert.match(manifest, /Mobilewallet\.xcframework/);
  assert.match(manifest, /ProcessInfo\.processInfo\.environment\["TARGET"\]/);
  assert.match(manifest, /ios-arm64_x86_64-simulator/);
  assert.match(manifest, /unsafeFlags\(\["-F", mobilewalletFrameworkPath\]\)/);
  assert.doesNotMatch(manifest, /\.binaryTarget\(/);
  const tauriConfig = JSON.parse(tauriConfigText);
  assert.deepEqual(tauriConfig.bundle.iOS.frameworks, [
    "../plugins/tauri-plugin-wallet-core/ios/Frameworks/Mobilewallet.xcframework",
  ]);
});

test("the Tauri bridge exposes only the bounded wallet command set", async () => {
  const [build, mobile, capability] = await Promise.all([
    load("build.rs"),
    load("src/mobile.rs"),
    readFile(path.join(root, "walletapp", "src-tauri", "capabilities", "mobile.json"), "utf8"),
  ]);
  for (const command of [
    "status", "create_wallet", "restore_wallet", "unlock", "lock", "receive", "activity",
    "preview_send", "confirm_send", "cancel_send", "recovery_phrase",
  ]) {
    assert.match(build, new RegExp(`"${command}"`));
  }
  assert.match(build, /cargo:rustc-link-search=framework=/);
  assert.match(build, /cargo:rustc-link-lib=framework=Mobilewallet/);
  assert.match(build, /ios-arm64_x86_64-simulator/);
  assert.match(mobile, /org\.bitcoin09\.walletcore/);
  assert.doesNotMatch(mobile, /shell|process|filesystem|http/);
  const parsed = JSON.parse(capability);
  assert.deepEqual(parsed.platforms, ["android", "iOS"]);
  assert.deepEqual(parsed.permissions, [
    "core:default",
    "wallet-core:default",
    "barcode-scanner:allow-check-permissions",
    "barcode-scanner:allow-request-permissions",
    "barcode-scanner:allow-scan",
  ]);
});

test("the Android host uses BTC09 launcher artwork rather than template icons", async () => {
  const host = path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "res");
  const sourceIcons = path.join(root, "walletapp", "src-tauri", "icons", "android");
  for (const density of ["mdpi", "hdpi", "xhdpi", "xxhdpi", "xxxhdpi"]) {
    for (const name of [
      "ic_launcher.png", "ic_launcher_foreground.png", "ic_launcher_background.png",
      "ic_launcher_monochrome.png", "ic_launcher_round.png",
    ]) {
      const [actual, expected] = await Promise.all([
        readFile(path.join(host, `mipmap-${density}`, name)),
        readFile(path.join(sourceIcons, `mipmap-${density}`, name)),
      ]);
      assert.deepEqual(actual, expected, `${density}/${name} drifted from BTC09 artwork`);
    }
  }
  const [adaptive, background, manifest] = await Promise.all([
    readFile(path.join(host, "mipmap-anydpi-v26", "ic_launcher.xml"), "utf8"),
    readFile(path.join(host, "values", "ic_launcher_background.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "AndroidManifest.xml"), "utf8"),
  ]);
  assert.match(adaptive, /@mipmap\/ic_launcher_foreground/);
  assert.match(adaptive, /@mipmap\/ic_launcher_background/);
  assert.match(adaptive, /@mipmap\/ic_launcher_monochrome/);
  assert.match(background, /ic_launcher_background/);
  assert.match(background, /#171a16/i);
  assert.match(manifest, /android:roundIcon="@mipmap\/ic_launcher_round"/);
});

test("platform icon generation keeps Apple corners opaque and Android layers adaptive", async () => {
  const sourceRoot = path.join(root, "walletapp", "src-tauri");
  const manifest = JSON.parse(await readFile(path.join(sourceRoot, "icon-manifest.json"), "utf8"));
  assert.equal(manifest.bg_color, "#171a16");
  assert.equal(manifest.android_bg, "icons/source/android-background.svg");
  assert.equal(manifest.android_fg, "icons/source/android-foreground.svg");
  assert.equal(manifest.android_monochrome, "icons/source/android-monochrome.svg");
  assert.ok(manifest.android_fg_scale >= 65 && manifest.android_fg_scale <= 80);
  const [background, foreground, monochrome] = await Promise.all([
    readFile(path.join(sourceRoot, manifest.android_bg), "utf8"),
    readFile(path.join(sourceRoot, manifest.android_fg), "utf8"),
    readFile(path.join(sourceRoot, manifest.android_monochrome), "utf8"),
  ]);
  assert.match(background, /fill="#171a16"/);
  assert.match(foreground, /stroke="#b35632"/);
  assert.doesNotMatch(foreground, /<rect[^>]+rx=/);
  assert.match(monochrome, /fill="#000000"|stroke="#000000"/);
});

test("Android wrapper pins the reviewed Gradle download", async () => {
  const [properties, rootGradle] = await Promise.all([
    readFile(path.join(
      root, "walletapp", "src-tauri", "gen", "android", "gradle", "wrapper", "gradle-wrapper.properties",
    ), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "build.gradle.kts"), "utf8"),
  ]);
  assert.match(properties, /gradle-8\.14\.3-bin\.zip/);
  assert.match(properties, /distributionSha256Sum=bd71102213493060956ec229d946beee57158dbd89d0e62b91bca0fa2c5f3531/);
  assert.match(rootGradle, /com\.android\.tools\.build:gradle:8\.11\.0/);
  assert.match(rootGradle, /org\.jetbrains\.kotlin:kotlin-gradle-plugin:2\.2\.21/);
});

test("Android release signing is opt-in and refuses partial credentials", async () => {
  const gradle = await readFile(path.join(
    root, "walletapp", "src-tauri", "gen", "android", "app", "build.gradle.kts",
  ), "utf8");
  for (const variable of [
    "BTC09_ANDROID_KEYSTORE",
    "BTC09_ANDROID_KEYSTORE_PASSWORD",
    "BTC09_ANDROID_KEY_ALIAS",
    "BTC09_ANDROID_KEY_PASSWORD",
  ]) assert.match(gradle, new RegExp(variable));
  assert.match(gradle, /hasAnyReleaseSigningValue/);
  assert.match(gradle, /hasAllReleaseSigningValues/);
  assert.match(gradle, /GradleException/);
  assert.match(gradle, /signingConfigs\.getByName\("release"\)/);
  assert.doesNotMatch(gradle, /storePassword\s*=\s*"[^$][^"]*"/);
  assert.doesNotMatch(gradle, /keyPassword\s*=\s*"[^$][^"]*"/);
});

test("QR scanning is mobile-only and uses the smallest camera surface", async () => {
  const [cargo, rust, capability, androidManifest, iosInfo] = await Promise.all([
    readFile(path.join(root, "walletapp", "src-tauri", "Cargo.toml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "src", "lib.rs"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "capabilities", "mobile.json"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "gen", "android", "app", "src", "main", "AndroidManifest.xml"), "utf8"),
    readFile(path.join(root, "walletapp", "src-tauri", "Info.ios.plist"), "utf8"),
  ]);
  assert.match(cargo, /cfg\(any\(target_os = "android", target_os = "ios"\)\)[\s\S]*tauri-plugin-barcode-scanner = "=2\.4\.5"/);
  assert.match(rust, /\.plugin\(tauri_plugin_barcode_scanner::init\(\)\)/);
  assert.match(androidManifest, /android\.permission\.CAMERA/);
  assert.match(androidManifest, /android\.permission\.VIBRATE" tools:node="remove"/);
  assert.match(androidManifest, /android\.hardware\.camera\.any"[\s\S]*android:required="false"/);
  assert.match(androidManifest, /android\.hardware\.camera" android:required="false"/);
  assert.match(iosInfo, /NSCameraUsageDescription/);
  assert.match(iosInfo, /Scan a BTC09 recipient address from a QR code\./);
  const permissions = JSON.parse(capability).permissions;
  for (const permission of [
    "barcode-scanner:allow-check-permissions",
    "barcode-scanner:allow-request-permissions",
    "barcode-scanner:allow-scan",
  ]) assert.ok(permissions.includes(permission), `missing ${permission}`);
  assert.ok(!permissions.some((permission) => /vibrate|open-app-settings|allow-cancel/.test(permission)));
});
