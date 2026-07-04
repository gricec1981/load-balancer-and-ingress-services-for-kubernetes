-- metering_flush.lua  (log phase: runs once per request, after response)
-- Sensor half of the split: SE enforces budgets, shim measures usage.
local ctx = ngx.ctx
if not ctx.tokens or ctx.tokens == 0 then return end

-- Identity: prefer x-ai-consumer (set by the SE AIGatewayAuthPolicy), then the
-- direct X-Auth-Sub (shim-direct/demo), then anonymous. Keying on the SE's
-- identity is what lets the dashboard's per-user counters line up.
local consumer = ngx.var.http_x_ai_consumer or ngx.var.http_x_auth_sub or "anonymous"
local model    = ngx.var.http_x_target_model or "unknown"
local key      = consumer .. ":" .. model

local m = ngx.shared.metering
m:incr(key, ctx.tokens, 0)
m:incr("requests:" .. key, 1, 0)
if ctx.killed then m:incr("killed:" .. key, 1, 0) end

ngx.log(ngx.INFO, "AI-SHIM metered ", key, " +", ctx.tokens,
        " tokens killed=", tostring(ctx.killed or false))
