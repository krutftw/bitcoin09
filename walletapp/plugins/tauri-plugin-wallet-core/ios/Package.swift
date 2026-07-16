// swift-tools-version:5.9

import PackageDescription

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
        .binaryTarget(
            name: "Mobilewallet",
            path: "Frameworks/Mobilewallet.xcframework"),
        .target(
            name: "tauri-plugin-wallet-core",
            dependencies: [
                .byName(name: "Tauri"),
                .byName(name: "Mobilewallet"),
            ],
            path: "Sources"),
    ]
)
