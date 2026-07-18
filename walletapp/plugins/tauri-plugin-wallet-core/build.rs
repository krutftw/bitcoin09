use std::{env, path::PathBuf};

const COMMANDS: &[&str] = &[
    "status",
    "create_wallet",
    "restore_wallet",
    "unlock",
    "lock",
    "receive",
    "activity",
    "preview_send",
    "confirm_send",
    "cancel_send",
    "recovery_phrase",
];

fn main() {
    tauri_plugin::Builder::new(COMMANDS)
        .android_path("android")
        .ios_path("ios")
        .build();

    if env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("ios") {
        // Keep the final Rust link on the same XCFramework slice that Package.swift
        // exposes while swift-rs compiles the native plugin.
        let target = env::var("TARGET").expect("Cargo did not provide the iOS target triple");
        let slice = if target == "aarch64-apple-ios" {
            "ios-arm64"
        } else if target == "x86_64-apple-ios" || target == "aarch64-apple-ios-sim" {
            "ios-arm64_x86_64-simulator"
        } else {
            panic!("Unsupported BTC09 iOS target: {target}");
        };
        let framework_dir = PathBuf::from(
            env::var("CARGO_MANIFEST_DIR").expect("Cargo did not provide the plugin directory"),
        )
        .join("ios/Frameworks/Mobilewallet.xcframework")
        .join(slice);
        if !framework_dir.join("Mobilewallet.framework/Mobilewallet").is_file() {
            panic!(
                "BTC09 mobile core is missing for {target}: {}",
                framework_dir.display()
            );
        }
        println!(
            "cargo:rustc-link-search=framework={}",
            framework_dir.display()
        );
        println!("cargo:rustc-link-lib=framework=Mobilewallet");
    }
}
