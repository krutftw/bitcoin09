import pathlib
import plistlib
import subprocess
import sys
import tempfile
import unittest
import zipfile


class MacOSPackageTest(unittest.TestCase):
    def test_builds_clickable_app_zip_with_executable_mode(self):
        repo = pathlib.Path(__file__).resolve().parents[2]
        script = repo / "tools" / "release" / "package_macos.py"
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            binary = root / "btc09-macos-apple"
            binary.write_bytes(b"fake Mach-O payload")
            output = root / "out"

            result = subprocess.run(
                [
                    sys.executable,
                    str(script),
                    "--binary",
                    str(binary),
                    "--arch",
                    "apple",
                    "--version",
                    "v0.1.29",
                    "--output-dir",
                    str(output),
                ],
                cwd=repo,
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            archive = output / "btc09-macos-apple.zip"
            self.assertTrue(archive.is_file())
            with zipfile.ZipFile(archive) as package:
                executable = package.getinfo(
                    "Bitcoin 09.app/Contents/MacOS/btc09"
                )
                self.assertEqual((executable.external_attr >> 16) & 0o777, 0o755)
                self.assertEqual(
                    package.read(executable),
                    b"fake Mach-O payload",
                )
                info = plistlib.loads(
                    package.read("Bitcoin 09.app/Contents/Info.plist")
                )
                self.assertEqual(info["CFBundleExecutable"], "btc09")
                self.assertEqual(info["CFBundleIdentifier"], "org.btc09.wallet")
                self.assertEqual(info["CFBundleShortVersionString"], "0.1.29")
                guide = package.read("README.txt").decode("utf-8")
                self.assertIn("Open Bitcoin 09.app", guide)
                self.assertIn("right-click", guide)


if __name__ == "__main__":
    unittest.main()
