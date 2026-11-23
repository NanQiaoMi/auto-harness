# auto-harness Python SDK

Zero-dependency Python 3.9+ SDK for driving `harness -rpc` and writing external process extensions.

## RPC Client

```python
from auto_harness_sdk import RPCClient

with RPCClient(["harness", "-rpc"]) as client:
    state = client.get_state()
    res = client.prompt("Explain this screenshot", images=[{"path": "./ui.png"}])
    print(res.text)
```

## Process Extension

```python
from auto_harness_sdk import ConsultResult, ExtensionServer, ToolResult

srv = ExtensionServer("my-ext", "Custom Python extension")

@srv.tool("hello", "Say hello", parameters={"type": "object", "properties": {"name": {"type": "string"}}})
def hello(args, call_id):
    return ToolResult(content=f"hello {args.get('name', 'world')}")

@srv.consult("tool_call")
def guard(params):
    if params["toolCall"]["name"] == "bash":
        return ConsultResult(block=True, reason="bash disabled")
    return ConsultResult()

srv.serve()
```

## Contract Test

```bash
python3 -m unittest discover -s sdk/python/tests -v
```
