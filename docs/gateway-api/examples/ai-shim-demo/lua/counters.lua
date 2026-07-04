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
        or k:find("^avgn:") or k == "redactions" or k == "cache_hits"
        or k == "cache_misses" or k == "gpu_ms_saved"
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

local users = {}
for u in (ngx.var.arg_users or ""):gmatch("[^,]+") do
    users[#users + 1] = u
end

local parts = {}
for _, u in ipairs(users) do
    parts[#parts + 1] = string.format('{"user":"%s","used":%d}', u, used_for(u))
end

-- shim-unique aggregate signals for the dashboard tiles
local function g(k) return m:get(k) or 0 end
ngx.say(string.format(
    '{"source":"ai-shim","counters":[%s],'
    .. '"redactions":%d,"cache_hits":%d,"cache_misses":%d,'
    .. '"gpu_seconds_saved":%.3f}',
    table.concat(parts, ","),
    g("redactions"), g("cache_hits"), g("cache_misses"), g("gpu_ms_saved") / 1000))
