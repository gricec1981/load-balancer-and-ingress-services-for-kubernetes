-- dlp.lua
-- Rule-based secret / PII detection + redaction for streamed output.
-- Dependency-free (stock OpenResty). Operates on PLAIN TEXT; the caller feeds
-- it an ACCUMULATED sliding window so a secret split across SSE frame
-- boundaries is still caught (never scan a single delta in isolation).
--
-- Tech-preview, rule-based. Guarantees cross-boundary catch for CONTIGUOUS
-- secrets up to MAX_TOKEN chars (card, ssn, email, api_key, aws_key). Secrets
-- interrupted by whitespace (e.g. "Bearer <tok>", spaced card groups) are
-- caught only when both halves land in the same scan window -- documented.

local M = {}
local byte, sub, gsub = string.byte, string.sub, string.gsub
local cjson = require "cjson.safe"

-- Longest contiguous secret we may need to hold back to complete a match
-- (AWS secret keys are 40 chars; keep margin). Drives the redact hold window.
M.MAX_TOKEN = 48

-- characters that can appear inside a secret (used to bound the held tail)
local ELIG = "[%w%-_%.@%+:/]"

-- Luhn checksum over a pure-digit string of plausible PAN length.
local function luhn_ok(d)
    local n = #d
    if n < 13 or n > 19 then return false end
    local sum, alt = 0, false
    for i = n, 1, -1 do
        local x = byte(d, i) - 48
        if x < 0 or x > 9 then return false end
        if alt then x = x * 2; if x > 9 then x = x - 9 end end
        sum = sum + x
        alt = not alt
    end
    return sum % 10 == 0
end

-- SSN validity (mirrors the ssn-strict WAF rule: reject reserved ranges).
local function ssn_ok(a, b, c)
    if a == "000" or a == "666" or sub(a, 1, 1) == "9" then return false end
    if b == "00" or c == "0000" then return false end
    return true
end

-- Redaction passes. Each replaces complete matches with [REDACTED:<type>] and
-- counts hits into stats. Run most-specific first. gsub over the accumulated
-- window is what makes boundary-straddling matches work.
local function pass(text, stats)
    local n = 0
    -- SSN (before bare digit runs; hyphens keep it distinct from a PAN)
    text = gsub(text, "(%d%d%d)%-(%d%d)%-(%d%d%d%d)", function(a, b, c)
        if ssn_ok(a, b, c) then n = n + 1; return "[REDACTED:ssn]" end
        return a .. "-" .. b .. "-" .. c
    end)
    -- credit card: any 13-19 digit run that passes Luhn
    text = gsub(text, "%d+", function(run)
        if luhn_ok(run) then n = n + 1; return "[REDACTED:pan]" end
        return run
    end)
    -- AWS access key id (AKIA/ASIA + 16 upper-alnum = 20 chars total)
    text = gsub(text, "A[SK]IA[A-Z0-9]+", function(m)
        if #m == 20 then n = n + 1; return "[REDACTED:aws_key]" end
        return m
    end)
    -- api keys: OpenAI-style sk-…, and the demo's SECRET-API-KEY-…
    text = gsub(text, "sk%-[%w]+", function() n = n + 1; return "[REDACTED:api_key]" end)
    text = gsub(text, "SECRET%-API%-KEY%-[%w]+",
        function() n = n + 1; return "[REDACTED:api_key]" end)
    -- bearer tokens (single window only; space-separated)
    text = gsub(text, "[Bb]earer%s+[%w%._%-]+",
        function() n = n + 1; return "Bearer [REDACTED:bearer]" end)
    -- email addresses
    text = gsub(text, "[%w%.%%%+%-]+@[%w%.%-]+%.%a%a+",
        function() n = n + 1; return "[REDACTED:email]" end)
    stats.n = (stats.n or 0) + n
    return text
end

-- redact(text) -> redacted_text, count
function M.redact(text)
    local stats = {}
    local out = pass(text, stats)
    return out, stats.n or 0
end

-- detect(text) -> true if any secret is present (kill-mode decision)
function M.detect(text)
    local _, n = M.redact(text)
    return n > 0
end

-- hold_start(s) -> index of the first char of the trailing eligible run
-- (the "incomplete match window" to withhold). Returns #s+1 when the buffer
-- ends on a non-secret char, i.e. nothing needs holding.
function M.hold_start(s)
    local i = #s
    while i >= 1 and sub(s, i, i):match(ELIG) do i = i - 1 end
    local start = i + 1
    if start > #s then return #s + 1 end
    if (#s - start + 1) > M.MAX_TOKEN then start = #s - M.MAX_TOKEN + 1 end
    return start
end

-- content_frame(text) -> a valid OpenAI-style SSE delta frame (cjson-encoded).
function M.content_frame(text)
    return "data: " ..
        cjson.encode({ choices = { { index = 0, delta = { content = text } } } }) ..
        "\n\n"
end

return M
