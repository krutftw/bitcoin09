import pathlib
import unittest


class DirectDistributionContractTest(unittest.TestCase):
    def test_public_guide_links_the_released_no_store_beta(self):
        guide_path = pathlib.Path("docs/DIRECT-DOWNLOADS.md")
        self.assertTrue(guide_path.exists(), "direct-download guide is missing")
        guide = guide_path.read_text(encoding="utf-8") if guide_path.exists() else ""

        for token in (
            "GitHub Releases",
            "SignPath Foundation",
            "wallet-only",
            "signed APK",
            "AppImage",
            "standard sideload permission",
            "macOS",
            "iPhone",
            "SHA256SUMS",
            "Available beta files",
            "releases/tag/v0.1.34",
            "btc09-wallet-android-arm64.apk",
            "btc09-wallet-linux-x64.AppImage",
        ):
            with self.subTest(token=token):
                self.assertIn(token, guide)

        self.assertNotIn("will use GitHub Releases", guide)

    def test_code_signing_policy_is_public_and_excludes_mining(self):
        policy_path = pathlib.Path("CODE_SIGNING.md")
        self.assertTrue(policy_path.exists(), "code-signing policy is missing")
        policy = policy_path.read_text(encoding="utf-8") if policy_path.exists() else ""

        for token in (
            "SignPath Foundation",
            "BTC09 Wallet",
            "wallet-only",
            "Mining software is outside",
            "Security reports",
            "GitHub Security Advisory",
            "Signing requests",
        ):
            with self.subTest(token=token):
                self.assertIn(token, policy)

        for forbidden in ("BEGIN PRIVATE KEY", "keystore password:", "seed phrase:"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, policy)

    def test_windows_direct_packager_is_wallet_only_and_fails_closed_on_signing(self):
        script_path = pathlib.Path("tools/release/package_windows_direct.ps1")
        self.assertTrue(script_path.exists(), "direct Windows packager is missing")
        script = script_path.read_text(encoding="utf-8") if script_path.exists() else ""

        for token in (
            "npm run store:build -- --bundles nsis",
            "verify-wallet-edition.mjs",
            "Get-AuthenticodeSignature",
            "AllowUnsignedPreflight",
            "signature is not trusted",
            "btc09-wallet-windows-x64-setup.exe",
        ):
            with self.subTest(token=token):
                self.assertIn(token, script)

        self.assertNotIn("Start mining", script)

    def test_homepage_links_the_released_native_wallet_beta(self):
        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        for token in (
            "Native wallet beta",
            "Windows signing",
            "Android signed APK",
            "Linux AppImage",
            "Not in this beta",
            "releases/download/v0.1.34/btc09-wallet-android-arm64.apk",
            "releases/download/v0.1.34/btc09-wallet-linux-x64.AppImage",
            "releases/download/v0.1.34/SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, homepage)

        native_status = homepage.split("Native wallet beta", 1)[-1].split("</aside>", 1)[0]
        self.assertNotIn("In preparation", native_status)

    def test_homepage_and_download_guide_disclose_the_signing_policy(self):
        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        guide = pathlib.Path("docs/DIRECT-DOWNLOADS.md").read_text(encoding="utf-8")
        required = (
            "Code signing policy",
            "Free code signing provided by",
            "SignPath.io",
            "certificate by SignPath Foundation",
        )
        for surface in (homepage, guide):
            for token in required:
                with self.subTest(token=token):
                    self.assertIn(token, surface)

    def test_mandatory_asert_upgrade_is_current_and_plain(self):
        surfaces = {
            "node": pathlib.Path("cmd/btc09/main.go").read_text(encoding="utf-8"),
            "wallet node": pathlib.Path("cmd/btc09/main_wallet.go").read_text(encoding="utf-8"),
            "tauri": pathlib.Path("walletapp/src-tauri/tauri.conf.json").read_text(encoding="utf-8"),
            "wallet package": pathlib.Path("walletapp/package.json").read_text(encoding="utf-8"),
            "appveyor": pathlib.Path("appveyor.yml").read_text(encoding="utf-8"),
            "exchange integration": pathlib.Path("docs/EXCHANGE-INTEGRATION.md").read_text(encoding="utf-8"),
        }
        for name, surface in surfaces.items():
            with self.subTest(surface=name):
                self.assertIn("0.1.34", surface)

        homepage = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        whitepaper = pathlib.Path("WHITEPAPER.md").read_text(encoding="utf-8")
        listing = pathlib.Path("docs/EXCHANGE-LISTING.md").read_text(encoding="utf-8")
        for surface in (homepage, whitepaper, listing):
            for token in ("12,096", "per-block ASERT"):
                with self.subTest(token=token):
                    self.assertIn(token, surface)
        self.assertIn("Mandatory update", homepage)
        self.assertNotIn(
            "Difficulty retargets every 2016 blocks, same as Bitcoin",
            whitepaper,
        )

    def test_v0134_release_notes_cover_the_consensus_upgrade_and_files(self):
        notes_path = pathlib.Path("docs/RELEASE-v0.1.34.md")
        self.assertTrue(notes_path.exists(), "v0.1.34 release notes are missing")
        notes = notes_path.read_text(encoding="utf-8") if notes_path.exists() else ""
        for token in (
            "Mandatory network update",
            "height 12,096",
            "per-block ASERT",
            "two-hour half-life",
            "Node and solo-miner operators",
            "Pool miners",
            "Wallet files and addresses do not change",
            "btc09-windows-amd64.exe",
            "btc09-linux-amd64",
            "btc09-linux-arm64",
            "btc09-macos-apple.zip",
            "btc09-macos-intel.zip",
            "btc09-wallet-android-arm64.apk",
            "btc09-wallet-linux-x64.AppImage",
            "SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, notes)

    def test_current_social_copy_does_not_repeat_the_retired_difficulty_rule(self):
        posts = pathlib.Path("POSTS.md").read_text(encoding="utf-8")
        self.assertIn("per-block ASERT from height 12,096", posts)
        self.assertNotIn("difficulty retarget every 2016 blocks", posts)

    def test_appveyor_builds_the_unsigned_windows_artifact_for_signpath(self):
        pipeline_path = pathlib.Path("appveyor.yml")
        self.assertTrue(pipeline_path.exists(), "free AppVeyor pipeline is missing")
        pipeline = pipeline_path.read_text(encoding="utf-8") if pipeline_path.exists() else ""
        toolchain = pathlib.Path("tools/release/install_appveyor_toolchain.ps1")
        build = pathlib.Path("tools/release/build_windows_appveyor.ps1")
        self.assertTrue(toolchain.exists(), "AppVeyor toolchain installer is missing")
        self.assertTrue(build.exists(), "AppVeyor Windows builder is missing")
        implementation = "\n".join(
            (
                pipeline,
                toolchain.read_text(encoding="utf-8") if toolchain.exists() else "",
                build.read_text(encoding="utf-8") if build.exists() else "",
            )
        )
        for token in (
            "Visual Studio 2022",
            "go1.25.12.windows-amd64.zip",
            "d5dc82da351b00e5eedd04f41356817d674cc4308131f0f638a5b14c5c3af4cb",
            "Install-Product node 24.16.0 x64",
            "rustup toolchain install 1.95.0 --profile minimal --no-self-update",
            "RUSTUP_TOOLCHAIN",
            "npm ci",
            "go test ./...",
            "package_windows_direct.ps1",
            "-AllowUnsignedPreflight",
            "btc09-wallet-windows-x64-setup.exe",
            "verify-wallet-edition.mjs",
        ):
            with self.subTest(token=token):
                self.assertIn(token, implementation)

        for forbidden in (
            "SIGNPATH_API_TOKEN",
            "BEGIN PRIVATE KEY",
            "signtool sign",
            "rustup default",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, implementation)

        self.assertLess(
            implementation.index("prepare-sidecar.mjs wallet"),
            implementation.index("cargo test --manifest-path walletapp/src-tauri/Cargo.toml"),
            "Tauri tests need the wallet-only sidecar before Cargo starts",
        )

    def test_appveyor_verifies_macos_and_iphone_without_publishing_artifacts(self):
        pipeline = pathlib.Path("appveyor.yml").read_text(encoding="utf-8")
        runner = pathlib.Path("tools/release/run_macos_appveyor.sh").read_text(
            encoding="utf-8"
        )
        for token in (
            "macos-sonoma",
            "bash tools/release/run_macos_appveyor.sh",
            "brew install go node@24 cocoapods",
            "rustup target add x86_64-apple-darwin aarch64-apple-darwin aarch64-apple-ios-sim",
            "go test ./...",
            "cargo test --manifest-path walletapp/src-tauri/Cargo.toml",
            "APPLE_SIGNING_IDENTITY=- npm --prefix walletapp run macos:universal:build",
            "node tools/desktop/verify-macos-bundle.mjs",
            "npm --prefix walletapp run mobile:core:ios",
            "npm run mobile:ios:simulator",
        ):
            with self.subTest(token=token):
                self.assertIn(token, f"{pipeline}\n{runner}")

        self.assertNotIn("appveyor PushArtifact", runner)
        self.assertNotIn("APPLE_PASSWORD", runner)
        self.assertNotIn("APPLE_TEAM_ID", runner)

    def test_appveyor_reuses_preinstalled_rustup_before_bootstrap(self):
        script = pathlib.Path(
            "tools/release/install_appveyor_toolchain.ps1"
        ).read_text(encoding="utf-8")
        rustup_probe = script.index("$rustup = Get-Command rustup")
        self.assertLess(
            script.index("$cargoBin ="),
            rustup_probe,
            "the AppVeyor image's Cargo bin directory must be known before probing rustup",
        )
        self.assertLess(
            script.index('$env:PATH = "$cargoBin;'),
            rustup_probe,
            "preinstalled rustup must be put on PATH before deciding to run rustup-init",
        )

    def test_appveyor_remaps_and_rejects_local_rust_build_paths(self):
        wrapper = pathlib.Path(
            "tools/release/run_windows_appveyor.ps1"
        ).read_text(encoding="utf-8")
        builder = pathlib.Path(
            "tools/release/build_windows_appveyor.ps1"
        ).read_text(encoding="utf-8")

        for token in (
            "$env:CARGO_ENCODED_RUSTFLAGS",
            "[char]0x1f",
            "--remap-path-prefix=$repoRoot=/src/bitcoin09",
            "--remap-path-prefix=$cargoHome=/cargo",
            "--remap-path-prefix=$rustupHome=/rustup",
        ):
            with self.subTest(token=token):
                self.assertIn(token, wrapper)

        self.assertLess(
            wrapper.index("$env:CARGO_ENCODED_RUSTFLAGS"),
            wrapper.index("build_windows_appveyor.ps1"),
            "Rust paths must be remapped before the Tauri build starts",
        )
        for token in (
            "Assert-NoLocalBuildPaths",
            "[System.Text.Encoding]::GetEncoding(28591)",
            "$env:USERPROFILE",
            "$walletExecutable",
            "$core",
        ):
            with self.subTest(token=token):
                self.assertIn(token, builder)

    def test_appveyor_rustup_bootstrap_uses_the_process_exit_code(self):
        script = pathlib.Path(
            "tools/release/install_appveyor_toolchain.ps1"
        ).read_text(encoding="utf-8")
        for token in (
            "Start-Process",
            "-Wait",
            "-PassThru",
            "$rustupInstall.ExitCode",
        ):
            with self.subTest(token=token):
                self.assertIn(token, script)
        self.assertNotIn(
            "& $rustupInit",
            script,
            "rustup-init writes non-fatal warnings to stderr on AppVeyor",
        )

    def test_appveyor_runs_native_tools_outside_the_ps_runner(self):
        pipeline = pathlib.Path("appveyor.yml").read_text(encoding="utf-8")
        wrapper_path = pathlib.Path("tools/release/run_windows_appveyor.ps1")
        self.assertTrue(wrapper_path.exists(), "the Windows AppVeyor wrapper is missing")
        wrapper = wrapper_path.read_text(encoding="utf-8") if wrapper_path.exists() else ""
        command = (
            "cmd: powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass "
            "-File tools\\release\\run_windows_appveyor.ps1"
        )
        self.assertIn(command, pipeline)
        self.assertNotIn("ps: ./tools/release/install_appveyor_toolchain.ps1", pipeline)
        self.assertNotIn("ps: ./tools/release/build_windows_appveyor.ps1", pipeline)
        self.assertLess(
            wrapper.index("install_appveyor_toolchain.ps1"),
            wrapper.index("build_windows_appveyor.ps1"),
            "the pinned toolchain must be installed before the Windows build starts",
        )

    def test_windows_packagers_capture_expected_native_failures_without_stderr_exceptions(self):
        helper_path = pathlib.Path("tools/release/invoke_process_capture.ps1")
        self.assertTrue(helper_path.exists(), "native process capture helper is missing")
        helper = helper_path.read_text(encoding="utf-8") if helper_path.exists() else ""
        for token in (
            "Start-Process",
            "-Wait",
            "-PassThru",
            "-RedirectStandardOutput",
            "-RedirectStandardError",
            "ExitCode",
        ):
            with self.subTest(token=token):
                self.assertIn(token, helper)

        for script_name in ("package_windows_direct.ps1", "package_windows_store.ps1"):
            script = pathlib.Path("tools/release", script_name).read_text(encoding="utf-8")
            with self.subTest(script=script_name):
                self.assertIn("invoke_process_capture.ps1", script)
                self.assertIn("Invoke-CapturedProcess", script)
                self.assertNotIn("(& $coreExecutable mine-pool 2>&1", script)

    def test_appveyor_normalizes_tauri_ci_environment(self):
        wrapper = pathlib.Path(
            "tools/release/run_windows_appveyor.ps1"
        ).read_text(encoding="utf-8")
        self.assertLess(
            wrapper.index("$env:CI = 'true'"),
            wrapper.index("build_windows_appveyor.ps1"),
            "Tauri must receive a lowercase boolean before its build starts",
        )

    def test_appveyor_binds_goroot_to_the_pinned_go_toolchain(self):
        script = pathlib.Path(
            "tools/release/install_appveyor_toolchain.ps1"
        ).read_text(encoding="utf-8")
        goroot_assignment = script.index("$env:GOROOT = $goRoot")
        go_validation = script.index("& go version")
        self.assertLess(
            goroot_assignment,
            go_validation,
            "Go must not reuse AppVeyor's preinstalled GOROOT",
        )
        self.assertIn(
            "Set-AppveyorBuildVariable -Name GOROOT -Value $env:GOROOT",
            script,
        )
        self.assertIn("& go env GOROOT", script)

    def test_gitlab_has_manual_reproducible_direct_package_jobs(self):
        pipeline = pathlib.Path(".gitlab-ci.yml").read_text(encoding="utf-8")
        for token in (
            "package-linux-appimage:",
            "package-android-apk:",
            "BTC09_ANDROID_KEYSTORE_B64",
            "BTC09_REQUIRE_ANDROID_SIGNATURE=1",
            "when: manual",
            "SHA256SUMS",
        ):
            with self.subTest(token=token):
                self.assertIn(token, pipeline)

    def test_gitlab_can_launch_check_an_exact_linux_release_artifact(self):
        pipeline = pathlib.Path(".gitlab-ci.yml").read_text(encoding="utf-8")
        for token in (
            "verify-linux-appimage:",
            "BTC09_LINUX_ARTIFACT_JOB_ID",
            "$CI_API_V4_URL/projects/$CI_PROJECT_ID/jobs/",
            "APPIMAGE_EXTRACT_AND_RUN=1",
            "xwininfo -root -tree",
            "BTC09 Wallet",
            "libasound2",
        ):
            with self.subTest(token=token):
                self.assertIn(token, pipeline)


if __name__ == "__main__":
    unittest.main()
