-- scan_timer.lua
-- Detached timer context: cosockets ARE allowed here (unlike body_filter).
-- Calls an external semantic classifier on accumulated output; writes a
-- verdict the NEXT chunk will act on => "one-chunk-lag" guardrail.
--
-- Demo default: rule check only (no external dep). To use a real classifier,
-- set CLASSIFIER_HOST/PORT and it will POST {"text": ...} and expect
-- {"verdict":"block"|"allow"}.

return function(premature, rid)
    if premature then return true end
    local text = ngx.shared.streamlog:get(rid)
    if not text or #text == 0 then return end
    if ngx.shared.verdicts:get(rid) then return true end  -- already decided

    local host = os.getenv("CLASSIFIER_HOST")
    if not host then
        -- inline heuristic fallback (keeps demo self-contained)
        if text:lower():find("wire the funds", 1, true) then
            ngx.shared.verdicts:set(rid, "block", 60)
        end
        return
    end

    local port = tonumber(os.getenv("CLASSIFIER_PORT") or 9090)
    local sock = ngx.socket.tcp()
    sock:settimeout(400)
    local ok = sock:connect(host, port)
    if not ok then return end

    local body = string.format('{"text": %q}', text)
    sock:send("POST /classify HTTP/1.1\r\nHost: " .. host ..
              "\r\nContent-Type: application/json\r\nContent-Length: " ..
              #body .. "\r\nConnection: close\r\n\r\n" .. body)
    local resp = sock:receive("*a")
    sock:close()
    if resp and resp:find('"verdict"%s*:%s*"block"') then
        ngx.shared.verdicts:set(rid, "block", 60)
    end
end
