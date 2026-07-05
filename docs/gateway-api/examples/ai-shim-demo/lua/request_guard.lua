-- request_guard.lua  (access phase: cosockets/subrequests allowed)
-- Synchronous request-side guardrail + semantic-cache serve path.
-- Replaces the ICAP REQMOD shim. The SE remains the policy point; this only
-- executes policy passed as request headers (X-DLP-Mode, X-Cache-Mode,
-- X-Token-Budget, X-Auth-Sub, X-Target-Model) -- it never decides identity.

local ctx = ngx.ctx
ctx.req_start = ngx.now()            -- generation-time basis for GPU-seconds saved

ngx.req.read_body()
local body = ngx.req.get_body_data()
if not body then return end

-- Inline injection signature (demo). A real deployment swaps this block
-- for the semantic classifier subrequest below.
if body:lower():find("ignore all previous instructions", 1, true) then
    ngx.status = 403
    ngx.header.content_type = "application/json"
    ngx.say('{"error":{"type":"prompt_injection_blocked",'
        .. '"enforced_by":"ako-ai-shim","phase":"request"}}')
    ngx.log(ngx.WARN, "AI-SHIM request blocked: injection signature")
    return ngx.exit(403)
end

-- ── cumulative token budget (SE-forwarded) ───────────────────────────────
-- The SE can't meter a streamed response, so its native cumulative gate is
-- inert on a streaming route. When the SE hands us the resolved budget +
-- window (X-Budget-Total / X-Budget-Window from the AITokenRateLimitPolicy),
-- the shim enforces it against a windowed per-consumer counter it maintains
-- (metering_flush increments it). All-or-nothing at admission, like the SE
-- native limiter: reject the next request once the window is at/over budget.
local btotal = tonumber(ngx.var.http_x_budget_total)
if btotal and btotal > 0 then
    local wsec = tonumber(ngx.var.http_x_budget_window) or 3600
    -- stash the SE-forwarded budget config so /v1/admin/counters can report
    -- usage-vs-budget to the dashboard. cfg_budget_total/window are the
    -- last-seen defaults; cfg_budget:<consumer> is the per-consumer ceiling
    -- (so per-user/per-group budgets render with the right ceiling each).
    ngx.shared.metering:set("cfg_budget_total", btotal)
    ngx.shared.metering:set("cfg_budget_window", wsec)
    local consumer = ngx.var.http_x_ai_consumer or ngx.var.http_x_auth_sub or "anonymous"
    ngx.shared.metering:set("cfg_budget:" .. consumer, btotal)
    local wb = math.floor(ngx.now() / wsec) * wsec
    local used = ngx.shared.metering:get("budget:" .. consumer .. ":" .. wb) or 0
    if used >= btotal then
        ngx.status = 429
        ngx.header.content_type = "application/json"
        ngx.header["X-AKO-Budget"] = "exceeded"
        ngx.say(string.format('{"error":{"type":"token_budget_exceeded",'
            .. '"used":%d,"budget":%d,"window_s":%d,"enforced_by":"ako-ai-shim"}}',
            used, btotal, wsec))
        ngx.log(ngx.WARN, "AI-SHIM cumulative budget exceeded consumer=", consumer,
            " used=", used, " budget=", btotal)
        return ngx.exit(429)
    end
end

-- ── semantic cache serve path ────────────────────────────────────────────
-- Only when the SE marks the route cacheable via X-Cache-Mode: on.
if (ngx.var.http_x_cache_mode or ""):lower() ~= "on" then return end

local cjson    = require "cjson.safe"
local dlp      = require "dlp"
local sha256   = require "resty.sha256"
local str      = require "resty.string"
local metering = ngx.shared.metering

local function hash(s)
    local h = sha256:new(); h:update(s or ""); return str.to_hex(h:final())
end

local req = cjson.decode(body) or {}
local msgs = req.messages or {}
local system, prompt = "", ""
for _, m in ipairs(msgs) do
    if m.role == "system" then system = m.content or system end
    if m.role == "user" then prompt = m.content or prompt end   -- last user wins
end
if prompt == "" then return end      -- nothing to key on; skip cache

local model  = ngx.var.http_x_target_model or req.model or "unknown"
local tenant = ngx.var.http_x_ai_consumer or ngx.var.http_x_auth_sub or "anonymous"
-- Cache namespace = model + tenant + system_hash. NEVER cross tenants.
ctx.cache_key = { model = model, tenant = tenant,
                  system_hash = hash(system), prompt = prompt }

local lookup = cjson.encode(ctx.cache_key)
local res = ngx.location.capture("/_cache_lookup",
    { method = ngx.HTTP_POST, body = lookup })

if not res or res.status ~= 200 then
    metering:incr("cache_misses", 1, 0)      -- miss (or cache down => fail-open)
    return                                    -- proceed to upstream; store on EOF
end

-- ── HIT: replay the cached text as OpenAI-style SSE from Lua ──────────────
local hit  = cjson.decode(res.body) or {}
local text = hit.response or ""
local words = {}
for w in text:gmatch("%S+") do words[#words + 1] = w end

ctx.cache_served = true               -- tell stream_filter to pass through
ngx.status = 200
ngx.header["Content-Type"] = "text/event-stream"
ngx.header["Cache-Control"] = "no-cache"
ngx.header["X-AKO-Cache"] = "hit"
ngx.header["X-AKO-AI-Shim"] = "active"

local i = 1
while i <= #words do
    local j = math.min(i + 3, #words)     -- ~4 words per frame
    local piece = table.concat(words, " ", i, j)
    if j < #words then piece = piece .. " " end
    ngx.print(dlp.content_frame(piece))
    ngx.flush(true)
    ngx.sleep(0.02)
    i = j + 1
end
ngx.print('data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],'
    .. '"usage":{"completion_tokens":' .. #words .. '}}\n\n')
ngx.print("data: [DONE]\n\n")
ngx.eof()

-- meter the hit + GPU-seconds saved (running avg generation time for model)
local key = tenant .. ":" .. model
metering:incr(key, #words, 0)
metering:incr("requests:" .. key, 1, 0)
metering:incr("cache_hits", 1, 0)
local tot = metering:get("avgtot:" .. model)
local n   = metering:get("avgn:" .. model)
if tot and n and n > 0 then metering:incr("gpu_ms_saved", tot / n, 0) end

ngx.log(ngx.INFO, "AI-SHIM cache HIT ", key, " words=", #words)
return ngx.exit(ngx.HTTP_OK)

-- Semantic classifier (synchronous — no lag on the request side):
-- local res = ngx.location.capture("/_classify",
--               { method = ngx.HTTP_POST, body = body })
-- if res.status == 200 and res.body:find('"verdict"%s*:%s*"block"') then
--     ngx.status = 403 ; ...
-- end
