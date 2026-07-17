import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, rmSync } from "node:fs";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const defaultRoot = path.resolve(scriptDirectory, "../..");

export function bindingPlan(target, platform = process.platform, root = defaultRoot) {
  if (target !== "android" && target !== "ios") {
    throw new Error("Mobile target must be android or ios.");
  }
  if (target === "ios" && platform !== "darwin") {
    throw new Error("The iPhone wallet core must be built on macOS with Xcode.");
  }
  const plugin = path.join(root, "walletapp", "plugins", "tauri-plugin-wallet-core");
  const toolDirectory = path.join(root, "walletapp", "src-tauri", "target", "mobile-tools");
  const gobind = path.join(toolDirectory, platform === "win32" ? "gobind.exe" : "gobind");
  const output = target === "android"
    ? path.join(plugin, "android", "libs", "mobilewallet.aar")
    : path.join(plugin, "ios", "Frameworks", "Mobilewallet.xcframework");
  const args = ["-trimpath"];
  if (target === "android") {
    args.push("-target=android", "-androidapi=24", "-javapkg=org.bitcoin09");
  } else {
    args.push("-target=ios", "-iosversion=13.0");
  }
  args.push(`-o=${output}`, "./mobilewallet");
  return {
    prepareCommand: ["go", "build", "-trimpath", `-o=${gobind}`, "golang.org/x/mobile/cmd/gobind"],
    command: ["go", "tool", "gomobile", "bind"],
    args,
    output,
    package: "./mobilewallet",
    root,
    toolDirectory,
  };
}

export function iosFrameworkFiles(output) {
  const framework = "Mobilewallet.framework";
  return ["ios-arm64", "ios-arm64_x86_64-simulator"].flatMap((slice) => {
    const root = path.join(output, slice, framework);
    return [path.join(root, "Mobilewallet"), path.join(root, "Modules", "module.modulemap")];
  });
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: defaultRoot,
    encoding: "utf8",
    stdio: options.capture ? "pipe" : "inherit",
    ...options,
  });
  if (result.error || result.status !== 0) {
    const detail = options.capture ? `${result.stdout || ""}${result.stderr || ""}`.trim() : "";
    throw new Error(detail || result.error?.message || `${command} failed.`);
  }
  return result;
}

async function checkBindings() {
  const root = await mkdtemp(path.join(tmpdir(), "btc09-mobile-bindings-"));
  try {
    const java = path.join(root, "java");
    const objc = path.join(root, "objc");
    run("go", ["tool", "gobind", "-lang=java", "-outdir", java, "./mobilewallet"]);
    run("go", ["tool", "gobind", "-lang=objc", "-outdir", objc, "./mobilewallet"]);
    const javaEngine = path.join(java, "java", "mobilewallet", "Engine.java");
    const objcHeader = path.join(objc, "src", "gobind", "Mobilewallet.objc.h");
    if (!existsSync(javaEngine) || !existsSync(objcHeader)) {
      throw new Error("The mobile binding generator did not produce both native APIs.");
    }
    const [javaSource, objcSource] = await Promise.all([
      readFile(javaEngine, "utf8"),
      readFile(objcHeader, "utf8"),
    ]);
    for (const method of ["createWallet", "restoreWallet", "unlock", "status", "receive", "activity", "previewSend", "confirmSend", "lock"]) {
      if (!javaSource.includes(method)) {
        throw new Error(`Android binding is missing ${method}.`);
      }
    }
    for (const method of ["createWallet", "restoreWallet", "unlock", "status", "receive", "activity", "previewSend", "confirmSend", "lock"]) {
      if (!objcSource.includes(method)) {
        throw new Error(`iPhone binding is missing ${method}.`);
      }
    }
    process.stdout.write("BTC09 mobile bindings are compatible with Android and iPhone APIs.\n");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

function build(target) {
  const plan = bindingPlan(target);
  mkdirSync(plan.toolDirectory, { recursive: true });
  mkdirSync(path.dirname(plan.output), { recursive: true });
  rmSync(plan.output, { recursive: true, force: true });
  run(plan.prepareCommand[0], plan.prepareCommand.slice(1));
  run(plan.command[0], [...plan.command.slice(1), ...plan.args], {
    env: {
      ...process.env,
      PATH: `${plan.toolDirectory}${path.delimiter}${process.env.PATH || ""}`,
    },
  });
  if (!existsSync(plan.output)) {
    throw new Error(`Mobile core output was not created at ${plan.output}.`);
  }
  if (target === "ios") {
    for (const required of iosFrameworkFiles(plan.output)) {
      if (!existsSync(required)) {
        throw new Error(`The iPhone wallet core is missing a required slice: ${required}`);
      }
    }
  }
  process.stdout.write(`Built ${plan.output}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const target = process.argv[2] || "check";
  try {
    if (target === "check") {
      await checkBindings();
    } else {
      build(target);
    }
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
