// Check your setup (run after `npm run build`):
//   CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 node hello-sandbox.mjs
import { crowdvmBox } from "./dist/index.js";

const backend = crowdvmBox({ name: "eve-hello", ttlSeconds: 600 });
const handle = await backend.create({ templateKey: null, sessionKey: "hello-session", runtimeContext: { appRoot: process.cwd() } });
try {
  const session = await handle.useSessionFn({ networkPolicy: "allow-all" });
  await session.writeTextFile({ path: "notes/hello.txt", content: "hello from CrowdVM\n" });
  const result = await session.run({ command: "cat notes/hello.txt && uname -a" });
  console.log("exit:", result.exitCode);
  console.log(result.stdout);
  console.log(session.resolvePath("notes/hello.txt")); // -> /workspace/notes/hello.txt
  const p = await session.spawn({ command: "for i in 1 2; do echo tick$i; sleep 0.2; done" });
  let out = ""; for await (const c of p.stdout) out += c;
  console.log("spawn:", JSON.stringify(out), await p.wait());
  await session.writeBinaryFile({ path: "bin.dat", content: new Uint8Array([0, 1, 2, 255]) });
  console.log("binary:", [...(await session.readBinaryFile({ path: "bin.dat" }))]);
  await session.removePath({ path: "notes" });
  console.log("after rm:", (await session.run({ command: "ls notes 2>&1; true" })).stdout.trim());
} finally {
  await handle.destroy();
}
