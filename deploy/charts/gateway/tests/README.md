# Gateway Chart Tests

## Running Lua unit tests

The `dns_utils.lua` module is intentionally free of OpenResty APIs so it can be
unit-tested with a standard Lua interpreter.

```bash
cd deploy/charts/gateway
LUA_PATH="tests/?.lua;;" lua tests/router_dns_test.lua
```

Expected output: all cases pass, e.g. `8/8 passed`.

`dns_utils.lua` is also embedded into the `nginx-router-config` ConfigMap via
Helm's `.Files.Get` helper, so the runtime nginx-router and the unit tests share
the same source file.
