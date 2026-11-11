#!/usr/bin/env node
// Prints the version to release on a channel.
//   stable:  VERSION as is (fails when that version is already on npm).
//   nightly: <base>-nightly.<yyyymmdd>.<run>, base = VERSION, or the patch
//            after the newest stable release when VERSION was released.
// Usage: version.mjs stable|nightly [run-number]
import { readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

const [channel, run = "0"] = process.argv.slice(2);
const want = readFileSync(new URL("../../VERSION", import.meta.url), "utf8").trim();
const parse = (v) => v.split("-")[0].split(".").map(Number);
const cmp = (a, b) => {
  const [x, y] = [parse(a), parse(b)];
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] - y[i];
  return 0;
};
let published = [];
try {
  published = JSON.parse(execFileSync("npm", ["view", "crowdvm", "versions", "--json"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }));
  if (!Array.isArray(published)) published = [published];
} catch {
  // not published yet
}
const stable = published.filter((v) => /^\d+\.\d+\.\d+$/.test(v)).sort(cmp);
const newest = stable.at(-1);

if (!/^\d+\.\d+\.\d+$/.test(want)) throw new Error(`VERSION must be x.y.z, got ${want}`);
if (channel === "stable") {
  if (published.includes(want)) throw new Error(`crowdvm ${want} is already released: bump VERSION on main`);
  process.stdout.write(want);
} else if (channel === "nightly") {
  let base = want;
  if (newest && cmp(newest, want) >= 0) {
    const [a, b, c] = parse(newest);
    base = `${a}.${b}.${c + 1}`;
  }
  const day = new Date().toISOString().slice(0, 10).replaceAll("-", "");
  process.stdout.write(`${base}-nightly.${day}.${run}`);
} else {
  throw new Error("usage: version.mjs stable|nightly [run-number]");
}
