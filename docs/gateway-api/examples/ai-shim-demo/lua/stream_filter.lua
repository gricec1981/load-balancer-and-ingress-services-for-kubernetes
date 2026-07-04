-- stream_filter.lua
-- Runs on EVERY response chunk (body_filter phase). This is the primitive the
-- SE lacks: inspect / meter / transform / truncate a streamed response without
-- buffering it. No cosockets here (phase restriction) -- async work goes to
-- timers (scan_timer.lua for guardrails, cache_client.lua for cache store).

local chunk, eof = ngx.arg[1], ngx.arg[2]
local ctx = ngx.ctx

-- A cache hit was already served from the access phase; let its frames flow
-- through untouched (no re-meter, no re-inspect).
if ctx.cache_served then return end

local dlp   = require "dlp"
local cjson = require "cjson.safe"

if not ctx.init then
    ctx.init     = true
    ctx.buf      = ""     -- SSE frame reassembly (chunk != frame boundary)
    ctx.tokens   = 0
    ctx.text     = ""     -- accumulated visible output (guardrails/scan/store)
    ctx.rwin     = ""     -- redaction hold window (redact mode only)
    ctx.killed   = false
    ctx.redacted = false
end

-- already killed: swallow whatever upstream keeps sending
if ctx.killed then
    ngx.arg[1] = ""
    return
end

local rid      = ngx.var.request_id
local verdicts = ngx.shared.verdicts
local metering = ngx.shared.metering
local dlp_mode = (ngx.var.http_x_dlp_mode or "kill"):lower()   -- kill|redact|off

local function kill_stream(reason)
    ctx.killed = true
    ngx.arg[1] = 'data: {"ako_gateway":{"terminated":true,"reason":"'
        .. reason .. '","tokens_delivered":' .. ctx.tokens
        .. '}}\n\ndata: [DONE]\n\n'
    ngx.arg[2] = true          -- signal EOF: client gets a clean close
    ngx.log(ngx.WARN, "AI-SHIM stream killed rid=", rid,
        " reason=", reason, " tokens=", ctx.tokens)
end

-- emit the "safe" (fully-arrived) prefix of the redaction window, redacted;
-- hold the trailing incomplete-match run. final=true flushes everything.
local function push_redacted(out, final)
    local s = ctx.rwin
    if #s == 0 then return end
    local start = final and (#s + 1) or dlp.hold_start(s)
    local safe = s:sub(1, start - 1)
    ctx.rwin = s:sub(start)
    if #safe > 0 then
        local red, n = dlp.redact(safe)
        if n > 0 then
            ctx.redacted = true
            metering:incr("redactions", n, 0)
        end
        out[#out + 1] = dlp.content_frame(red)
    end
end

-- 1) async guardrail verdict from the timer-based scanner (one-chunk lag)
if verdicts:get(rid) == "block" then
    return kill_stream("output_guardrail")
end

local redact_out = (dlp_mode == "redact") and {} or nil

if chunk and #chunk > 0 then
    ctx.buf = ctx.buf .. chunk

    -- 2) reassemble complete SSE frames (delimited by a blank line)
    while true do
        local s, e = ctx.buf:find("\n\n", 1, true)
        if not s then break end
        local frame = ctx.buf:sub(1, s - 1)
        ctx.buf = ctx.buf:sub(e + 1)

        local payload = frame:match("^data: (.+)$")
        if payload and payload ~= "[DONE]" then
            ctx.tokens = ctx.tokens + 1                 -- ~1 token per delta
            local total = payload:match('"completion_tokens"%s*:%s*(%d+)')
            if total then ctx.tokens = tonumber(total) end
            local piece = payload:match('"content"%s*:%s*"(.-[^\\])"')
                or payload:match('"content"%s*:%s*""')
            if piece then
                ctx.text = ctx.text .. piece
                if redact_out then ctx.rwin = ctx.rwin .. piece end
            elseif redact_out then
                -- non-content (finish/usage) frame: flush window, pass it on
                push_redacted(redact_out, true)
                redact_out[#redact_out + 1] = "data: " .. payload .. "\n\n"
            end
        elseif payload == "[DONE]" and redact_out then
            push_redacted(redact_out, true)
            redact_out[#redact_out + 1] = "data: [DONE]\n\n"
        end
    end

    -- 3) output guardrails (injection is a guardrail regardless of DLP mode)
    if ctx.text:lower():find("ignore all previous instructions", 1, true) then
        return kill_stream("output_injection")
    end
    -- 3b) DLP kill mode: secret in output => terminate immediately
    if dlp_mode == "kill" and dlp.detect(ctx.text) then
        return kill_stream("output_dlp")
    end

    -- 4) mid-stream token budget (SE passes X-Token-Budget on the request)
    local budget = tonumber(ngx.var.http_x_token_budget)
    if budget and ctx.tokens >= budget then
        return kill_stream("token_budget_exceeded")
    end

    -- 5) hand accumulated text to the async semantic scanner
    ngx.shared.streamlog:set(rid, ctx.text, 60)
    if not ctx.scan_scheduled then
        ctx.scan_scheduled = true
        local ok, err = ngx.timer.every(0.25, require("scan_timer"), rid)
        if not ok then ngx.log(ngx.ERR, "scan timer failed: ", err) end
    end

    -- 6) DLP redact mode: replace upstream bytes with reconstructed frames,
    --    holding at most the trailing incomplete-match window (~one frame).
    if redact_out then
        push_redacted(redact_out, false)
        ngx.arg[1] = table.concat(redact_out)
    end
end

-- flush any held window + finalize cache capture on EOF
if eof then
    if redact_out and #ctx.rwin > 0 then
        local out = {}
        push_redacted(out, true)
        ngx.arg[1] = (ngx.arg[1] or "") .. table.concat(out)
    end

    -- running average generation time per model (basis for GPU-seconds saved).
    -- Only a real generation counts -- not a cache replay.
    if not ctx.killed and ctx.tokens > 0 and ctx.req_start then
        local model = ngx.var.http_x_target_model or "unknown"
        local gen_ms = (ngx.now() - ctx.req_start) * 1000
        metering:incr("avgtot:" .. model, gen_ms, 0)
        metering:incr("avgn:" .. model, 1, 0)
    end

    -- capture -> store, but NEVER cache a killed or DLP-redacted response.
    if ctx.cache_key and not ctx.killed and not ctx.redacted then
        ctx.cache_key.response = ctx.text
        local payload = cjson.encode(ctx.cache_key)
        local ok, err = ngx.timer.at(0, require("cache_client").store, payload)
        if not ok then ngx.log(ngx.ERR, "cache store timer failed: ", err) end
    end
end
