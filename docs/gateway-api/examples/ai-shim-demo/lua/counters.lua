-- counters.lua : shim-served /v1/admin/counters (dashboard data source).
-- The SE can't count streamed tokens (it never buffers the stream), so the shim
-- is the source of truth. This returns REAL per-consumer streamed token usage
-- from the metering shared dict, in the same JSON shape as the SE admin endpoint
-- so the console reads it unchanged. Gated by X-Admin-Token (a control-plane
-- read; the dashboard calls it in-cluster, not through the SE data path).

ngx.header.content_type = "application/json"

local admin = os.getenv("ADMIN_TOKEN")
if not admin or admin == "" or ngx.var.http_x_admin_token ~= admin then
    ngx.status = 403
    ngx.say('{"error":"forbidden"}')
    return
end

local m = ngx.shared.metering

-- meta keys in the dict that are NOT "<consumer>:<model>" token counters
local function is_meta(k)
    return k:find("^requests:") or k:find("^killed:") or k:find("^avgtot:")
        or k:find("^avgn:") or k:find("^budget:") or k:find("^cfg_")
        or k == "redactions" or k == "cache_hits" or k == "cache_misses"
        or k == "gpu_ms_saved"
end

-- sum streamed tokens for a consumer across all its models
local function used_for(user)
    local prefix, total = user .. ":", 0
    for _, k in ipairs(m:get_keys(0)) do
        if not is_meta(k) and k:sub(1, #prefix) == prefix then
            total = total + (m:get(k) or 0)
        end
    end
    return total
end

-- windowed cumulative budget (option 1): the last budget/window the SE
-- forwarded (stashed by request_guard). window_used is this consumer's usage in
-- the current window -- what the shim enforces the cumulative 429 against.
local budget_total  = m:get("cfg_budget_total") or 0
local budget_window = m:get("cfg_budget_window") or 0
local wb = (budget_window > 0) and (math.floor(ngx.now() / budget_window) * budget_window) or 0
local function window_used_for(user)
    if budget_window <= 0 then return 0 end
    return m:get("budget:" .. user .. ":" .. wb) or 0
end

local users = {}
for u in (ngx.var.arg_users or ""):gmatch("[^,]+") do
    users[#users + 1] = u
end

local parts = {}
for _, u in ipairs(users) do
    local ubudget = m:get("cfg_budget:" .. u) or budget_total
    parts[#parts + 1] = string.format('{"user":"%s","used":%d,"window_used":%d,"budget":%d}',
        u, used_for(u), window_used_for(u), ubudget)
end

-- shim-unique aggregate signals for the dashboard tiles
local function g(k) return m:get(k) or 0 end
ngx.say(string.format(
    '{"source":"ai-shim","budget_total":%d,"budget_window_s":%d,"counters":[%s],'
    .. '"redactions":%d,"cache_hits":%d,"cache_misses":%d,'
    .. '"gpu_seconds_saved":%.3f}',
    budget_total, budget_window, table.concat(parts, ","),
    g("redactions"), g("cache_hits"), g("cache_misses"), g("gpu_ms_saved") / 1000))
