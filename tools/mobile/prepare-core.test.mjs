import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";

import { bindingPlan, iosFrameworkFiles } from "./prepare-core.mjs";

test("Android binding is a pinned, app-private AAR with a modern minimum API", () => {
  const plan = bindingPlan("android", "win32", "C:/repo");

  assert.deepEqual(plan.prepareCommand.slice(0, 3), ["go", "build", "-trimpath"]);
  assert.match(plan.prepareCommand[3], /-o=.*mobile-tools[\\/]gobind\.exe$/);
  assert.equal(plan.prepareCommand[4], "golang.org/x/mobile/cmd/gobind");
  assert.doesNotMatch(plan.prepareCommand.join(" "), /@latest/);
  assert.match(plan.toolDirectory, /src-tauri[\\/]target[\\/]mobile-tools$/);
  assert.deepEqual(plan.command, ["go", "tool", "gomobile", "bind"]);
  assert.ok(plan.args.includes("-target=android"));
  assert.ok(plan.args.includes("-androidapi=24"));
  assert.ok(plan.args.includes("-javapkg=org.bitcoin09"));
  assert.match(plan.output, /android[\\/]libs[\\/]mobilewallet\.aar$/);
  assert.equal(plan.package, "./mobilewallet");
});

test("Apple binding produces one iPhone and simulator XCFramework on macOS", () => {
  const plan = bindingPlan("ios", "darwin", "/repo");

  assert.ok(plan.args.includes("-target=ios"));
  assert.ok(plan.args.includes("-iosversion=13.0"));
  assert.match(plan.output, /ios[\\/]Frameworks[\\/]Mobilewallet\.xcframework$/);
  assert.deepEqual(
    iosFrameworkFiles("/tmp/Mobilewallet.xcframework"),
    [
      "/tmp/Mobilewallet.xcframework/ios-arm64/Mobilewallet.framework/Mobilewallet",
      "/tmp/Mobilewallet.xcframework/ios-arm64/Mobilewallet.framework/Modules/module.modulemap",
      "/tmp/Mobilewallet.xcframework/ios-arm64_x86_64-simulator/Mobilewallet.framework/Mobilewallet",
      "/tmp/Mobilewallet.xcframework/ios-arm64_x86_64-simulator/Mobilewallet.framework/Modules/module.modulemap",
    ].map((file) => file.replaceAll("/", path.sep)),
  );
});

test("Apple binding refuses to pretend it can build on Windows", () => {
  assert.throws(() => bindingPlan("ios", "win32", "C:/repo"), /macOS with Xcode/);
  assert.throws(() => bindingPlan("unknown", "linux", "/repo"), /android or ios/);
});
