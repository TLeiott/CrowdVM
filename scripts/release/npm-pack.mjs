#!/usr/bin/env node
// Lays out the npm packages of one release under OUT/npm:
//   cli-<os>-<cpu>/  @crowdvm/cli-<os>-<cpu>: one platform's binary
//   crowdvm/         crowdvm: the `crowdvm` command (a launcher that runs the
//                    binary of the platform package npm installed)
//   sdk/             @crowdvm/sdk: the TypeScript client (built)
// Usage: npm-pack.mjs VERSION OUT   (OUT/bin from build-cli.sh)
import { cpSync, mkdirSync, readFileSync, rmSync, writeFileSync, chmodSync } from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";

const [version, out] = process.argv.slice(2);
if (!version || !out) throw new Error("usage: npm-pack.mjs VERSION OUT");
const root = path.resolve(new URL("../..", import.meta.url).pathname);
const npm = path.join(out, "npm");
const license = path.join(root, "LICENSE-npm");
const repository = { type: "git", url: "git+https://github.com/TLeiott/CrowdVM.git" };
const common = {
  version,
  license: "SEE LICENSE IN LICENSE",
  author: "Tim Leiott (https://github.com/TLeiott)",
  homepage: "https://github.com/TLeiott/CrowdVM",
  repository,
};
// Go GOOS/GOARCH -> npm process.platform/process.arch
const platforms = [
  ["linux", "amd64", "linux", "x64"],
  ["linux", "arm64", "linux", "arm64"],
  ["darwin", "amd64", "darwin", "x64"],
  ["darwin", "arm64", "darwin", "arm64"],
  ["windows", "amd64", "win32", "x64"],
  ["windows", "arm64", "win32", "arm64"],
];
const write = (file, data) => writeFileSync(file, typeof data === "string" ? data : JSON.stringify(data, null, 2) + "\n");

rmSync(npm, { recursive: true, force: true });
const optional = {};
for (const [goos, goarch, os, cpu] of platforms) {
  const name = `@crowdvm/cli-${os}-${cpu}`;
  const dir = path.join(npm, `cli-${os}-${cpu}`);
  const exe = os === "win32" ? "crowdvm.exe" : "crowdvm";
  mkdirSync(path.join(dir, "bin"), { recursive: true });
  cpSync(path.join(out, "bin", `${goos}-${goarch}`, exe), path.join(dir, "bin", exe));
  chmodSync(path.join(dir, "bin", exe), 0o755);
  cpSync(license, path.join(dir, "LICENSE"));
  write(path.join(dir, "package.json"), {
    name, ...common,
    description: `The crowdvm CLI binary for ${os}-${cpu}. Installed by the crowdvm package; use that.`,
    os: [os], cpu: [cpu], files: ["bin", "LICENSE"],
    preferUnplugged: true,
  });
  write(path.join(dir, "README.md"), `# ${name}\n\nThe \`crowdvm\` binary for ${os}-${cpu}. Install [\`crowdvm\`](https://www.npmjs.com/package/crowdvm) instead: it picks this package for you.\n`);
  optional[name] = version;
}

// The command: a launcher that runs the platform binary.
const cli = path.join(npm, "crowdvm");
mkdirSync(path.join(cli, "bin"), { recursive: true });
write(path.join(cli, "bin", "crowdvm.js"), `#!/usr/bin/env node
"use strict";
// Runs the crowdvm binary that npm installed for this platform
// (an optional dependency: @crowdvm/cli-<platform>-<arch>).
const { spawn } = require("node:child_process");
const exe = process.platform === "win32" ? "crowdvm.exe" : "crowdvm";
const pkg = "@crowdvm/cli-" + process.platform + "-" + process.arch;
let bin;
try {
  bin = require.resolve(pkg + "/bin/" + exe);
} catch {
  console.error("crowdvm: " + pkg + " is not installed. Supported: " + ${JSON.stringify(Object.keys(optional).map((n) => n.replace("@crowdvm/cli-", "")).join(", "))} + ".\\n" +
    "Reinstall without --omit=optional (or --no-optional): npm i -g crowdvm");
  process.exit(1);
}
// The binary knows npm runs it: updates go through npm, not self-update.
const child = spawn(bin, process.argv.slice(2), { stdio: "inherit", env: { ...process.env, CROWDVM_NPM_LAUNCHER: __filename } });
// The terminal sends Ctrl-C (and Ctrl-\\\\) to the whole process group: the
// binary handles it. Other signals are passed on.
for (const s of ["SIGINT", "SIGQUIT"]) process.on(s, () => {});
for (const s of ["SIGTERM", "SIGHUP"]) process.on(s, () => child.kill(s));
child.on("error", (e) => { console.error("crowdvm: could not run " + bin + ": " + e.message); process.exit(1); });
child.on("exit", (code, signal) => {
  if (signal) { process.removeAllListeners(signal); process.kill(process.pid, signal); }
  process.exit(code ?? 1);
});
`);
chmodSync(path.join(cli, "bin", "crowdvm.js"), 0o755);
cpSync(license, path.join(cli, "LICENSE"));
write(path.join(cli, "package.json"), {
  name: "crowdvm", ...common,
  description: "CrowdVM CLI: cloud sandboxes for coding agents on your own machines (LAN and Tailscale).",
  keywords: ["crowdvm", "sandbox", "vm", "agents", "cli"],
  bin: { crowdvm: "bin/crowdvm.js" },
  files: ["bin", "LICENSE", "README.md"],
  engines: { node: ">=18" },
  optionalDependencies: optional,
});
write(path.join(cli, "README.md"), `# crowdvm

The CrowdVM command line: cloud sandboxes for coding agents, on machines you
and your team lend to a self-hosted CrowdVM server.

\`\`\`sh
npm i -g crowdvm                 # stable
npm i -g crowdvm@nightly         # nightly builds
crowdvm login --server http://<your-crowdvm-server>:8420
\`\`\`

\`crowdvm ssh\` and port forwards use your system's OpenSSH client.

Free for personal and other non-commercial use; commercial use needs
permission. See LICENSE.
`);

// The TypeScript client, built from sdk/typescript.
const sdkSrc = path.join(root, "sdk", "typescript");
const sdk = path.join(npm, "sdk");
execFileSync("npm", ["ci", "--no-audit", "--no-fund"], { cwd: sdkSrc, stdio: "inherit" });
execFileSync("npm", ["run", "build"], { cwd: sdkSrc, stdio: "inherit" });
const base = JSON.parse(readFileSync(path.join(sdkSrc, "package.json"), "utf8"));
mkdirSync(sdk, { recursive: true });
cpSync(path.join(sdkSrc, "dist"), path.join(sdk, "dist"), { recursive: true });
cpSync(path.join(sdkSrc, "README.md"), path.join(sdk, "README.md"));
cpSync(license, path.join(sdk, "LICENSE"));
delete base.scripts;
delete base.devDependencies;
write(path.join(sdk, "package.json"), { ...base, ...common, name: "@crowdvm/sdk", files: ["dist", "README.md", "LICENSE"] });
console.log(`npm packages for ${version} in ${npm}`);
