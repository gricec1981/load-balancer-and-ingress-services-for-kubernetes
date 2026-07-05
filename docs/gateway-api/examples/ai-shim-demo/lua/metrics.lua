-- metrics.lua : Prometheus-style exposition of shim counters.
-- Avi can scrape this and join it into VS analytics via the info-metric
-- pattern; the SE rate limiter enforces budgets against the token totals.
ngx.header.content_type = "text/plain"
local m = ngx.shared.metering
local avgtot, avgn, lines = {}, {}, {}

for _, key in ipairs(m:get_keys(0)) do
    local v = m:get(key) or 0
    if key:find("^requests:") then
        lines[#lines + 1] = 'ai_shim_requests_total{key="' .. key:sub(10) .. '"} ' .. v
    elseif key:find("^killed:") then
        lines[#lines + 1] = 'ai_shim_streams_killed_total{key="' .. key:sub(8) .. '"} ' .. v
    elseif key == "redactions" then
        lines[#lines + 1] = 'ai_shim_redactions_total ' .. v
    elseif key == "cache_hits" then
        lines[#lines + 1] = 'ai_shim_cache_hits_total ' .. v
    elseif key == "cache_misses" then
        lines[#lines + 1] = 'ai_shim_cache_misses_total ' .. v
    elseif key == "gpu_ms_saved" then
        lines[#lines + 1] = 'ai_shim_gpu_seconds_saved ' .. string.format("%.3f", v / 1000)
    elseif key:find("^avgtot:") then
        avgtot[key:sub(8)] = v
    elseif key:find("^avgn:") then
        avgn[key:sub(6)] = v
    elseif key:find("^budget:") then
        lines[#lines + 1] = 'ai_shim_budget_window_used{key="' .. key:sub(8) .. '"} ' .. v
    elseif key:find("^cfg_") then
        -- forwarded budget config (not a metric) -- skip
    else
        lines[#lines + 1] = 'ai_shim_tokens_total{key="' .. key .. '"} ' .. v
    end
end

-- running average generation time per model (basis for GPU-seconds saved)
for model, tot in pairs(avgtot) do
    local n = avgn[model] or 0
    if n > 0 then
        lines[#lines + 1] = 'ai_shim_avg_generation_seconds{model="' .. model ..
            '"} ' .. string.format("%.3f", (tot / n) / 1000)
    end
end

for _, l in ipairs(lines) do ngx.say(l) end
