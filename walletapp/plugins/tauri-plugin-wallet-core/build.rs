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
}
