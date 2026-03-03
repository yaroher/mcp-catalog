---
status: investigating
trigger: "Debug why gemini cli mcp list shows 'mcp-catalog-proxy' as Disconnected even though it works correctly when tested manually with echo | mcp-manager --mcp-stdio. It seems initialize and tools/list responses are correct. Check for any hidden errors, timeouts, or protocol mismatches."
created: 2024-05-24T12:00:00Z
updated: 2024-02-26T14:19:00Z
---

## Current Focus
hypothesis: The `mcp-manager` fails to parse JSON-RPC requests when the `id` field is a string, because it's hardcoded as `int` in the `rpcReq` struct.
test: Sent an `initialize` request with a string ID and observed a `parse error` response.
expecting: Confirmed that string IDs cause failure.
next_action: Fix `rpcReq` and `rpcResp` to handle both string and integer IDs.

## Symptoms
expected: 'mcp-catalog-proxy' should show as Connected in gemini cli mcp list.
actual: 'mcp-catalog-proxy' shows as Disconnected.
errors: `{"jsonrpc":"2.0","id":0,"error":{"code":-32700,"message":"parse error"}}` when string ID is used.
reproduction: `echo '{"jsonrpc":"2.0","id":"test-id","method":"initialize",...}' | ./build/mcp-manager --mcp-stdio`
started: Unknown.

## Eliminated

## Evidence
- 2024-02-26T14:18:00Z: Tested manual `initialize` with string ID. Found that it returns `parse error`.
- 2024-02-26T14:18:00Z: Inspected `internal/server/mcp_proxy.go` and found `rpcReq` and `rpcResp` have `ID int`.

## Resolution
root_cause: JSON-RPC IDs can be strings or numbers, but the implementation only supports integers. Many MCP clients use string IDs.
fix: 
verification: 
files_changed: []
