// swift-tools-version:5.9

import PackageDescription
import Foundation

// Tauri's swift-rs bridge invokes `swift build`, whose iOS CLI path does not
// expose local XCFramework binary targets. Point Swift at the matching slice;
// build.rs supplies the same framework to the final Rust link.
let rustTarget = ProcessInfo.processInfo.environment["TARGET"] ?? ""
let mobilewalletSlice = rustTarget == "aarch64-apple-ios"
    ? "ios-arm64"
    : "ios-arm64_x86_64-simulator"
let packageRoot = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
let mobilewalletFrameworkPath = packageRoot
    .appendingPathComponent("Frameworks/Mobilewallet.xcframework")
    .appendingPathComponent(mobilewalletSlice)
    .path

let package = Package(
    name: "tauri-plugin-wallet-core",
    platforms: [.iOS(.v13)],
    products: [
        .library(
            name: "tauri-plugin-wallet-core",
            type: .static,
            targets: ["tauri-plugin-wallet-core"]),
    ],
    dependencies: [
        .package(name: "Tauri", path: "../.tauri/tauri-api"),
    ],
    targets: [
        .target(
            name: "tauri-plugin-wallet-core",
            dependencies: [
                .byName(name: "Tauri"),
            ],
            path: "Sources",
            swiftSettings: [
                .unsafeFlags(["-F", mobilewalletFrameworkPath]),
            ],
            linkerSettings: [
                .unsafeFlags(["-F", mobilewalletFrameworkPath]),
                .linkedFramework("Mobilewallet"),
            ]),
    ]
)
