# auto-harness TypeScript SDK

Zero-dependency TypeScript SDK for Node 22+ (`--experimental-strip-types` / native type-stripping) covering both `harness -rpc` clients and stdio JSON-RPC process extensions.

## RPC Client

```ts
import { RPCClient } from "./src/index.ts";

const client = new RPCClient(["harness", "-rpc"]);
client.start();
const res = await client.prompt("Describe this image", {
  images: [{ path: "./ui.png" }],
});
console.log(res.text);
await client.close();
```

## Process Extension

```ts
import { ExtensionServer } from "./src/index.ts";

const srv = new ExtensionServer("my-ts-ext", "Custom TS extension");
srv.registerTool(
  { name: "hello", description: "Say hello" },
  (args) => ({ content: `hello ${args.name ?? "world"}` }),
);
await srv.serve();
```

## Contract Test

```bash
node --experimental-strip-types --test sdk/typescript/test/contract.test.ts
```
