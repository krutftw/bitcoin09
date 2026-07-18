# BTC09 glib security backport

This directory contains the official `glib` 0.18.5 crate with the upstream
`VariantStrIter::impl_get` security fix backported. It is used only by the
Linux Tauri/GTK wallet dependency graph.

- Upstream crate: https://crates.io/crates/glib/0.18.5
- Archive SHA-256: `233daaf6e83ae6a12a52055f568f9d7cf4671dabb78ff9560ab6da230ce00ee5`
- Advisory: https://github.com/advisories/GHSA-wrw7-89jp-8q8g
- Upstream fix: https://github.com/gtk-rs/gtk-rs-core/pull/1343
- Upstream merge commit: `05dff0ee696f9bcd8617cd48c4b812d046d440cb`
- License: MIT, preserved in `LICENSE`

The backport changes the C out-argument from `&p` to `&mut p` and makes the
pointer binding mutable. No other upstream source is modified.

Tauri 2.11 still depends on the GTK3 crate line that requires `glib` 0.18, so
the patched upstream `glib` 0.20 release cannot replace it through a normal
semver update. Remove this override when the supported Tauri Linux dependency
graph moves to `glib` 0.20 or newer.
