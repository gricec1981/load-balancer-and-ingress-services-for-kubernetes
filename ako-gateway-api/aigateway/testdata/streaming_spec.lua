-- Behavioural spec for streamed completions against the SE sandbox stub. Run by
-- TestStreamingScriptsAgainstSEStub, which writes the generated phases into
-- <dir> first: req / native_req (HTTP_REQ), reqdata (HTTP_REQ_DATA), resp
-- (HTTP_RESP), respdata / native_respdata (HTTP_RESP_DATA), plus two
-- HTTP_REQ_DATA variants — reqdata_default (defaultMaxTokens 256) and
-- reqdata_deny (mode Deny).
--
-- What these prove that no string match can: that a streamed request lands
-- its reservation in the SAME keys a measured completion would (window
-- counter, native carry), that the ledger row says "estimated", that an error
-- hands the reservation back, that a stream the request phase never saw is
-- charged the fail-closed penalty, and that the native gate trips on the
-- reserved figure at the consumer's next request.

local dir = arg[1] or error("usage: streaming_spec.lua <dir-with-generated-scripts>")
local SE = dofile(dir .. "/se_stub.lua")

local function slurp(name)
  local f = assert(io.open(dir .. "/" .. name, "rb"))
  local s = f:read("a")
  f:close()
  return s
end

local REQ, NREQ, REQDATA = slurp("req.lua"), slurp("native_req.lua"), slurp("reqdata.lua")
local RESP, RESPDATA, NRESPDATA = slurp("resp.lua"), slurp("respdata.lua"), slurp("native_respdata.lua")
local REQDATA_DEFAULT, REQDATA_DENY = slurp("reqdata_default.lua"), slurp("reqdata_deny.lua")

-- ── Assertions ───────────────────────────────────────────────────────────────

local failures = 0
local function check(ok, msg, got, want)
  if ok then return end
  failures = failures + 1
  io.write("FAIL: ", msg)
  if want ~= nil then io.write("\n      got:  ", tostring(got), "\n      want: ", tostring(want)) end
  io.write("\n")
end
local function eq(got, want, msg) check(got == want, msg, got, want) end

-- ── Fixtures ─────────────────────────────────────────────────────────────────

-- {"sub":"alice","group":"group1"}
local JWT = "hdr.eyJzdWIiOiJhbGljZSIsImdyb3VwIjoiZ3JvdXAxIn0.sig"
local WINDOW = 3600

local function wb() return math.floor(SE.env.now / WINDOW) * WINDOW end
local function tv(k) local e = SE.T[k]; if e and e.exp > SE.env.now then return tonumber(e.v) or 0 end return 0 end
local function group_counter() return tv("7:hourly-group-budget:alice:" .. wb()) end
local function native_counter() return tv("7:native-hourly:alice:" .. wb()) end
local function carry() return tv("tkcarry:native-hourly:alice") end

-- Live records in the ring, the highest sequence, and how many there are. The
-- ring TTL is five minutes and fresh() moves the clock by hours, so counts —
-- not sequence numbers — are what a scenario compares.
local function records()
  local out, last, n = {}, 0, 0
  for k, e in pairs(SE.T) do
    if string.sub(k, 1, 8) == "ai_urec:" and e.exp > SE.env.now then
      local seq = tonumber(string.sub(k, 9))
      out[seq] = e.v
      n = n + 1
      if seq > last then last = seq end
    end
  end
  return out, last, n
end
local function nrecords() local _, _, n = records(); return n end
local function last_record()
  local recs, last = records()
  return recs[last], last
end
local function split(s)
  local out, start = {}, 1
  while true do
    local p = string.find(s, "|", start, true)
    if not p then out[#out + 1] = string.sub(s, start) break end
    out[#out + 1] = string.sub(s, start, p - 1)
    start = p + 1
  end
  return out
end
local function logged(sub)
  for _, l in ipairs(SE.env.logs) do if string.find(l, sub, 1, true) then return true end end
  return false
end

local COMPLETION = '{"id":"chatcmpl-1","object":"chat.completion","model":"qwen3-14b-awq",' ..
  '"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],' ..
  '"usage":{"prompt_tokens":31,"completion_tokens":17,"total_tokens":48}}'
local NONSTREAM = '{"model":"qwen3-14b","messages":[{"role":"user","content":"hello"}],"max_tokens":100}'
local STREAM = '{"model":"qwen3-14b","messages":[{"role":"user","content":"hello"}],"stream": true,"max_tokens":100}'
local EST = math.floor(#STREAM / 4)
local JSON = { ["Content-Type"] = "application/json" }
local SSE = { ["Content-Type"] = "text/event-stream" }

-- One request through every phase, in SE execution order. Each phase stops the
-- walk when it answered the request itself.
local function request(over, reqdata)
  SE.reset_request(over)
  SE.env.query = "jwt=" .. JWT
  SE.run("req", REQ)
  if SE.env.resp then return SE.env.resp end
  SE.run("native_req", NREQ)
  if SE.env.resp then return SE.env.resp end
  SE.run("reqdata", reqdata or REQDATA)
  if SE.env.resp then return SE.env.resp end
  SE.run("resp", RESP)
  SE.run("respdata", RESPDATA)
  SE.run("native_respdata", NRESPDATA)
  return nil
end

-- A fresh window and a fresh limiter, so scenarios do not bleed into each other.
local function fresh()
  SE.env.now = SE.env.now + 2 * WINDOW
  SE.RL = {}
  SE.rl_budget = 1e12
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. A non-streamed completion is untouched by the streaming code: metered
--    exactly from the body, no stream flags, and the native carry holds the
--    measured figure.
-- ─────────────────────────────────────────────────────────────────────────────
do
  request({ reqbody = NONSTREAM, body = COMPLETION, respheaders = JSON })
  eq(group_counter(), 48, "a plain completion still charges its measured total")
  eq(carry(), 48, "a plain completion still stages its measured total in the native carry")
  eq(SE.env.reqvars["ai_stream"], nil, "a plain completion sets no stream flag")
  eq(split(last_record() or "")[9], "exact", "a plain completion is still recorded as exact")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. A streamed request reserves prompt-estimate + max_tokens at admission, in
--    the same keys, and is recorded as an estimate — the stream itself is never
--    buffered.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local before = nrecords()
  local resp = request({ reqbody = STREAM, respheaders = SSE })
  eq(resp, nil, "a streamed request with max_tokens is admitted")
  eq(group_counter(), EST + 100, "the reservation is charged to the DataScript window counter")
  eq(native_counter(), EST + 100, "the reservation is charged to the native display counter")
  eq(carry(), EST + 100, "the reservation is staged in the native carry for the next request's gate")
  eq(SE.env.reqvars["ai_stream"], "1", "the request is flagged as a reserved stream")
  eq(SE.env.reqvars["ai_skip_meter"], "1", "the response phases are told to leave the stream alone")
  eq(SE.env.buffered, nil, "the response body is never buffered for a stream")
  check(logged("stream=1 mode=reserve reserved=" .. (EST + 100)), "the reservation is named in the client log")

  local r = last_record()
  eq(nrecords(), before + 1, "a streamed response appends exactly one record")
  local f = split(r or "")
  eq(f[9], "estimated", "a reservation is recorded as 'estimated', never as a measurement")
  eq(f[2], "alice", "the estimated record carries the identity")
  eq(f[3], "qwen3-14b", "the estimated record carries the model the client asked for")
  eq(f[5], tostring(EST), "the estimated record carries the prompt estimate")
  eq(f[6], "100", "the estimated record carries max_tokens as the completion figure")
  eq(f[10], "llm-route", "the estimated record carries the route")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. No max_tokens means no bound. The default policy refuses the request
--    before routing; a policy with defaultMaxTokens reserves that instead.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local body = '{"model":"qwen3-14b","messages":[{"role":"user","content":"hi"}],"stream":true}'
  local resp = request({ reqbody = body, respheaders = SSE })
  check(resp ~= nil and resp.code == 400, "a stream without max_tokens is refused", resp and resp.code, 400)
  check(resp and string.find(resp.body, "max_tokens_required", 1, true) ~= nil, "the refusal names the reason")
  eq(group_counter(), 0, "a refused stream reserves nothing")

  fresh()
  resp = request({ reqbody = body, respheaders = SSE }, REQDATA_DEFAULT)
  eq(resp, nil, "with defaultMaxTokens the same request is admitted")
  eq(carry(), math.floor(#body / 4) + 256, "and reserves the policy default")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Whitespace around the field is fine, and n multiplies the ceiling.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local body = '{"model":"qwen3-14b","messages":[],"n": 3, "stream" : true , "max_tokens" : 100}'
  request({ reqbody = body, respheaders = SSE })
  eq(carry(), math.floor(#body / 4) + 300, "n choices reserve n × max_tokens")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. An error instead of a stream hands the reservation back.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local before = nrecords()
  request({ reqbody = STREAM, status = 503, body = '{"error":"upstream"}', respheaders = JSON })
  eq(group_counter(), 0, "a failed stream releases the window counter")
  eq(carry(), 0, "a failed stream releases the native carry")
  eq(nrecords(), before, "a failed stream leaves no record")
  check(logged("stream=1 released="), "the release is named in the client log")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. A stream the request phase never saw — the field sat past the buffered
--    head — is charged the fail-closed penalty, exactly like an unreadable body.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local body = '{"model":"m","messages":[{"role":"user","content":"' .. string.rep("a", 40000) ..
    '"}],"stream":true,"max_tokens":50}'
  request({ reqbody = body, respheaders = SSE })
  eq(SE.env.reqvars["ai_stream"], nil, "a stream field past the 32 KB head is not seen at admission")
  eq(group_counter(), 65536, "an unreserved stream is charged the fail-closed penalty")
  eq(carry(), 65536, "the penalty reaches the native carry too")
  eq(split(last_record() or "")[9], "penalty", "an unreserved stream is recorded as a penalty")
  check(logged("stream=1 undetected penalty="), "the backstop is named in the client log")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. The native gate enforces the reservation at the consumer's next request:
--    one behind, like a measured completion.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  SE.rl_budget = 200
  local r1 = request({ reqbody = STREAM, respheaders = SSE })
  eq(r1, nil, "first stream: bucket untouched beyond the probe, admitted")
  local r2 = request({ reqbody = STREAM, respheaders = SSE })
  eq(r2, nil, "second stream: the first reservation (" .. (EST + 100) .. ") fits in 200, admitted")
  local r3 = request({ reqbody = NONSTREAM, body = COMPLETION, respheaders = JSON })
  check(r3 ~= nil and r3.code == 429, "third request: the second reservation does not fit, rejected", r3 and r3.code, 429)
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. Deny mode answers 400 before routing; a plain request is untouched.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local resp = request({ reqbody = STREAM, respheaders = SSE }, REQDATA_DENY)
  check(resp ~= nil and resp.code == 400, "Deny refuses a streamed request", resp and resp.code, 400)
  check(resp and string.find(resp.body, "streaming_not_allowed", 1, true) ~= nil, "the refusal names the reason")
  eq(group_counter(), 0, "Deny charges nothing")

  fresh()
  resp = request({ reqbody = NONSTREAM, body = COMPLETION, respheaders = JSON }, REQDATA_DENY)
  eq(resp, nil, "Deny admits a non-streamed request")
  eq(group_counter(), 48, "and it is metered exactly as before")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. "stream": false is not a stream.
-- ─────────────────────────────────────────────────────────────────────────────
do
  fresh()
  local body = '{"model":"qwen3-14b","messages":[],"stream":false,"max_tokens":100}'
  request({ reqbody = body, body = COMPLETION, respheaders = JSON })
  eq(SE.env.reqvars["ai_stream"], nil, "stream:false sets no flag")
  eq(group_counter(), 48, "stream:false is metered from the body")
end

if failures > 0 then
  io.write(failures, " assertion(s) failed\n")
  os.exit(1)
end
io.write("streaming spec: all assertions passed\n")
