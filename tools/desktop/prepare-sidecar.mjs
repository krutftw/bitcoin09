import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const targets = new Map([
  ["x86_64-pc-windows-msvc", { goos: "windows", goarch: "amd64", extension: ".exe" }],
  ["aarch64-pc-windows-msvc", { goos: "windows", goarch: "arm64", extension: ".exe" }],
  ["x86_64-pc-windows-gnu", { goos: "windows", goarch: "amd64", extension: ".exe" }],
  ["x86_64-apple-darwin", { goos: "darwin", goarch: "amd64", extension: "" }],
  ["aarch64-apple-darwin", { goos: "darwin", goarch: "arm64", extension: "" }],
  ["x86_64-unknown-linux-gnu", { goos: "linux", goarch: "amd64", extension: "" }],
  ["aarch64-unknown-linux-gnu", { goos: "linux", goarch: "arm64", extension: "" }],
  ["x86_64-unknown-linux-musl", { goos: "linux", goarch: "amd64", extension: "" }],
  ["aarch64-unknown-linux-musl", { goos: "linux", goarch: "arm64", extension: "" }],
]);

export function targetToGo(target) {
  const config = targets.get(target);
  if (!config) {
    throw new Error(`Unsupported desktop target: ${target}`);
  }
  return { ...config };
}

export function assertMatchingVersions(shellVersion, goSource) {
  const match = goSource.match(/const\s+nodeVersion\s*=\s*"v([0-9]+\.[0-9]+\.[0-9]+)"/);
  if (!match) {
    throw new Error("Could not read the BTC09 Core release version.");
  }
  if (match[1] !== shellVersion) {
    throw new Error(`BTC09 Wallet ${shellVersion} does not match BTC09 Core ${match[1]}.`);
  }
}

export function goBuildArguments(output, edition = "full") {
  if (edition !== "full" && edition !== "wallet") {
    throw new Error(`Unsupported desktop edition: ${edition}`);
  }
  const args = ["build", "-trimpath"];
  if (edition === "wallet") args.push("-tags", "walletedition");
  args.push("-o", output, "./cmd/btc09");
  return args;
}

function commandOutput(command, args, cwd) {
  const result = spawnSync(command, args, { cwd, encoding: "utf8", windowsHide: true });
  if (result.status !== 0) {
    throw new Error(result.stderr?.trim() || `${command} failed.`);
  }
  return result.stdout;
}

export function hostTarget(root) {
  if (process.env.TAURI_ENV_TARGET_TRIPLE) {
    return process.env.TAURI_ENV_TARGET_TRIPLE;
  }
  const targetIndex = process.argv.indexOf("--target");
  if (targetIndex >= 0 && process.argv[targetIndex + 1]) {
    return process.argv[targetIndex + 1];
  }
  const details = commandOutput("rustc", ["-vV"], root);
  const match = details.match(/^host:\s*(\S+)$/m);
  if (!match) {
    throw new Error("Could not determine the Rust desktop target.");
  }
  return match[1];
}

export function buildSidecar(root, edition = "full") {
  const target = hostTarget(root);
  const platform = targetToGo(target);
  const cargoSource = readFileSync(path.join(root, "walletapp", "src-tauri", "Cargo.toml"), "utf8");
  const shellVersion = cargoSource.match(/^version\s*=\s*"([^"]+)"/m)?.[1];
  if (!shellVersion) {
    throw new Error("Could not read the BTC09 Wallet release version.");
  }
  const goSource = readFileSync(path.join(root, "cmd", "btc09", "main.go"), "utf8");
  assertMatchingVersions(shellVersion, goSource);

  const binaryDirectory = path.join(root, "walletapp", "src-tauri", "binaries");
  mkdirSync(binaryDirectory, { recursive: true });
  const output = path.join(binaryDirectory, `btc09-core-${target}${platform.extension}`);
  const result = spawnSync(
    "go",
    goBuildArguments(output, edition),
    {
      cwd: root,
      env: { ...process.env, GOOS: platform.goos, GOARCH: platform.goarch, CGO_ENABLED: "0" },
      stdio: "inherit",
      windowsHide: true,
    },
  );
  if (result.status !== 0) {
    throw new Error("BTC09 Core could not be built for the desktop app.");
  }
  return { output, target, version: shellVersion, edition };
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
  const root = path.resolve(scriptDirectory, "..", "..");
  const edition = process.argv[2] || "full";
  const result = buildSidecar(root, edition);
  process.stdout.write(`Prepared BTC09 Core ${result.version} (${result.edition}) for ${result.target}.\n`);
}
