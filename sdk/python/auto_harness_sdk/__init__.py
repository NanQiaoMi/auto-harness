"""Zero-dependency Python SDK for auto-harness RPC and process extensions."""

from .extension import ConsultResult, ExtensionServer, ToolResult
from .rpc import PromptResult, RPCClient, RPCError, SCHEMA_VERSION

__all__ = [
    "ConsultResult",
    "ExtensionServer",
    "PromptResult",
    "RPCClient",
    "RPCError",
    "SCHEMA_VERSION",
    "ToolResult",
]
