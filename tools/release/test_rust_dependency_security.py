import pathlib
import tomllib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
TAURI_MANIFEST = ROOT / "walletapp" / "src-tauri" / "Cargo.toml"
TAURI_LOCK = ROOT / "walletapp" / "src-tauri" / "Cargo.lock"
VENDORED_GLIB = ROOT / "walletapp" / "vendor" / "glib-0.18.5-btc09"


class RustDependencySecurityTests(unittest.TestCase):
    def test_patched_glib_source_is_locked(self):
        manifest = tomllib.loads(TAURI_MANIFEST.read_text(encoding="utf-8"))
        patch = manifest["patch"]["crates-io"]["glib"]
        self.assertEqual(patch, {"path": "../vendor/glib-0.18.5-btc09"})

        lock = tomllib.loads(TAURI_LOCK.read_text(encoding="utf-8"))
        packages = [package for package in lock["package"] if package["name"] == "glib"]
        self.assertEqual(len(packages), 1)
        self.assertEqual(packages[0]["version"], "0.18.5")
        self.assertNotIn("source", packages[0])
        self.assertNotIn("checksum", packages[0])

    def test_variant_string_iterator_uses_mutable_out_pointer(self):
        source = (VENDORED_GLIB / "src" / "variant_iter.rs").read_text(encoding="utf-8")
        self.assertIn("let mut p: *mut libc::c_char = std::ptr::null_mut();", source)
        self.assertIn("&mut p,", source)
        self.assertNotIn("\n                &p,", source)

    def test_backport_provenance_is_recorded(self):
        provenance = (VENDORED_GLIB / "BTC09-PATCH.md").read_text(encoding="utf-8")
        self.assertIn("GHSA-wrw7-89jp-8q8g", provenance)
        self.assertIn("05dff0ee696f9bcd8617cd48c4b812d046d440cb", provenance)
        self.assertIn("233daaf6e83ae6a12a52055f568f9d7cf4671dabb78ff9560ab6da230ce00ee5", provenance)


if __name__ == "__main__":
    unittest.main()
