import pathlib
import unittest


class CIWorkflowContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow_path = pathlib.Path(".github/workflows/ci.yml")
        cls.workflow = cls.workflow_path.read_text(encoding="utf-8")

    def test_ci_runs_for_changes_and_manual_recovery(self):
        for token in ("push:", "pull_request:", "workflow_dispatch:"):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_ci_is_read_only_and_cancels_superseded_runs(self):
        for token in (
            "contents: read",
            "cancel-in-progress: true",
            "github.workflow",
            "github.ref",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_go_quality_gate_is_complete(self):
        for token in (
            "actions/checkout@v4",
            "actions/setup-go@v5",
            "go-version-file: go.mod",
            "golang/govulncheck-action@v1",
            "go-package: ./...",
            "go vet ./...",
            "go test -race ./...",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_project_contracts_cover_public_surfaces(self):
        for token in (
            "actions/setup-node@v4",
            "node-version: 24",
            "actions/setup-python@v5",
            'python-version: "3.13"',
            "node --test tools/discord/*.test.mjs",
            "node --test nineinbox/web/*.test.mjs",
            "node --test tools/desktop/*.test.mjs tools/desktop/*.test.cjs",
            "python -m unittest discover -s bot/tests -p 'test_*.py'",
            "python -m unittest \\",
            "tools.release.test_package_macos",
            "tools.site.test_index_contract",
            "tools.deploy.test_nine_inbox_contract",
            "tools.deploy.test_open_miner_contract",
            "tools.deploy.test_web_presence_contract",
            "tools.exchange.test_btc09_exchange_smoke",
            "tools.ci.test_ci_workflow_contract",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)

    def test_native_wallet_is_checked_on_each_desktop_platform(self):
        for token in (
            "name: Native wallet",
            "windows-latest",
            "macos-14",
            "ubuntu-22.04",
            "libwebkit2gtk-4.1-dev",
            "libayatana-appindicator3-dev",
            "dtolnay/rust-toolchain@stable",
            "cargo fmt --manifest-path walletapp/src-tauri/Cargo.toml -- --check",
            "cargo test --manifest-path walletapp/src-tauri/Cargo.toml",
            "npm run desktop:build -- --no-bundle",
            "npm run store:build -- --no-bundle",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.workflow)


if __name__ == "__main__":
    unittest.main()
