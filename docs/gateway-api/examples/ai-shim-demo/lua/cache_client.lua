-- cache_client.lua
-- Cache STORE over a raw TCP cosocket. Called ONLY from a zero-delay timer:
-- cosockets are forbidden in the body_filter phase, so the capture-on-EOF
-- handoff schedules this. Posts the captured full response to the semantic
-- cache /store endpoint. (The serve/LOOKUP side uses ngx.location.capture in
-- the access phase -- see request_guard.lua.)

local M = {}

-- store(premature, payload)  payload = pre-serialized JSON body for /store
function M.store(premature, payload)
    if premature then return end
    local backend = os.getenv("CACHE_BACKEND")
    if not backend then return end
    local host, port = backend:match("^([^:]+):?(%d*)$")
    port = tonumber(port) or 9200

    local sock = ngx.socket.tcp()
    sock:settimeout(1000)
    local ok, err = sock:connect(host, port)
    if not ok then
        ngx.log(ngx.WARN, "AI-SHIM cache store connect failed: ", err)
        return
    end
    local req = "POST /store HTTP/1.1\r\nHost: " .. host ..
        "\r\nContent-Type: application/json\r\nContent-Length: " .. #payload ..
        "\r\nConnection: close\r\n\r\n" .. payload
    local _, serr = sock:send(req)
    if serr then
        ngx.log(ngx.WARN, "AI-SHIM cache store send failed: ", serr)
        sock:close()
        return
    end
    sock:receive("*l")   -- consume status line, then close cleanly
    sock:close()
    ngx.log(ngx.INFO, "AI-SHIM cache stored ", #payload, " bytes")
end

return M
