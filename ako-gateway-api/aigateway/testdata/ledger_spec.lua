-- Behavioural spec for the generated token-accounting DataScripts, executed
-- against the SE sandbox stub. Run by TestGeneratedScriptsAgainstSEStub, which
-- writes the generated phases into <dir> first.
--
-- These are the assertions that cannot be made by matching strings in Go: what
-- the counter actually holds after a request, whether a read-only request costs
-- anything, and whether a record survives the drain with its fields intact.

local dir = arg[1] or error("usage: ledger_spec.lua <dir-with-generated-scripts>")
local SE = dofile(dir .. "/se_stub.lua")

local function slurp(name)
  local f = assert(io.open(dir .. "/" .. name, "rb"))
  local s = f:read("a")
  f:close()
  return s
end

local REQ, RESP, RESPDATA = slurp("req.lua"), slurp("resp.lua"), slurp("respdata.lua")

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

-- A JWT whose payload decodes to {"sub":"alice","group":"group1"}. The scripts
-- only base64url-decode the middle segment; the SE has already validated it.
local JWT = "hdr.eyJzdWIiOiJhbGljZSIsImdyb3VwIjoiZ3JvdXAxIn0.sig"

local WINDOW = 3600
local function counter_key()
  local wb = math.floor(SE.env.now / WINDOW) * WINDOW
  return "7:hourly-group-budget:alice:" .. wb
end
local function counter()
  local e = SE.T[counter_key()]
  return e and tonumber(e.v) or 0
end

local COMPLETION = '{"id":"chatcmpl-1","object":"chat.completion","created":1,' ..
  '"model":"qwen3-14b-awq","choices":[{"index":0,"message":{"role":"assistant",' ..
  '"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,' ..
  '"completion_tokens":17,"total_tokens":48,' ..
  '"prompt_tokens_details":{"cached_tokens":12},' ..
  '"completion_tokens_details":{"reasoning_tokens":5}}}'

-- One full request through all three phases.
local function request(over)
  SE.reset_request(over)
  SE.env.query = "jwt=" .. JWT .. (SE.env.extraquery or "")
  SE.run("req", REQ)
  if SE.env.resp then return SE.env.resp end -- rejected or answered in HTTP_REQ
  SE.run("resp", RESP)
  SE.run("respdata", RESPDATA)
  return nil
end

local function records()
  local out = {}
  for k, e in pairs(SE.T) do
    if string.sub(k, 1, 8) == "ai_urec:" and e.exp > SE.env.now then
      out[tonumber(string.sub(k, 9))] = e.v
    end
  end
  return out
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

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. A real completion is metered exactly, and recorded with its dimensions.
-- ─────────────────────────────────────────────────────────────────────────────
do
  local before = counter()
  request({ body = COMPLETION, respheaders = { ["Content-Type"] = "application/json" } })

  eq(counter() - before, 48, "a completion charges its total_tokens to the budget")

  local r = records()[1]
  check(r ~= nil, "a completion appends one usage record")
  if r then
    local f = split(r)
    eq(#f, 10, "record has all ten fields")
    eq(f[2], "alice", "record carries the JWT sub as identity")
    eq(f[3], "qwen3-14b-awq", "record carries the model the backend served")
    eq(f[5], "31", "record carries prompt tokens")
    eq(f[6], "17", "record carries completion tokens")
    eq(f[7], "12", "record carries cached prompt tokens as their own dimension")
    eq(f[8], "5", "record carries reasoning tokens as their own dimension")
    eq(f[9], "exact", "a parsed usage block is provenance 'exact'")
    eq(f[10], "llm-route", "record carries the HTTPRoute it was measured on")
  end
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. A read-only request costs nothing. This is the regression that mattered:
--    GET /v1/models used to be charged the full 65536 fail-closed penalty and
--    exhausted a 500-token budget outright.
-- ─────────────────────────────────────────────────────────────────────────────
do
  local before, recs_before = counter(), 0
  for _ in pairs(records()) do recs_before = recs_before + 1 end

  request({
    path = "/v1/models",
    body = '{"object":"list","data":[{"id":"qwen3-14b-awq","object":"model"}]}',
    respheaders = { ["Content-Type"] = "application/json" },
  })

  eq(counter(), before, "a short JSON response with no usage costs nothing")
  local n = 0
  for _ in pairs(records()) do n = n + 1 end
  eq(n, recs_before, "a response that was never a completion appends no record")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. A body too large to buffer is still fail-closed — that is the genuine
--    bypass the penalty exists to shut — but it is recorded as a penalty, not
--    as a measurement.
-- ─────────────────────────────────────────────────────────────────────────────
do
  local before = counter()
  -- 256 KB of filler, so `usage` sits past the end of the buffer.
  local huge = '{"id":"x","model":"qwen3-14b-awq","choices":[{"text":"' ..
    string.rep("a", 300 * 1024) .. '"}],"usage":{"total_tokens":9}}'
  request({ body = huge, respheaders = { ["Content-Type"] = "application/json" } })

  eq(counter() - before, 65536, "an over-buffer response is charged the fail-closed penalty")

  local last, r = 0, nil
  for seq, v in pairs(records()) do if seq > last then last, r = seq, v end end
  if r then eq(split(r)[9], "penalty", "an over-buffer response is recorded as a penalty, not a measurement") end
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Enforcement still fails closed: the penalty above blew the 500-token
--    group budget, so the next request is rejected in HTTP_REQ.
-- ─────────────────────────────────────────────────────────────────────────────
do
  local resp = request({ body = COMPLETION, respheaders = { ["Content-Type"] = "application/json" } })
  check(resp ~= nil and resp.code == 429, "a consumer over budget is rejected",
    resp and resp.code or "not rejected")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. The drain hands the records to the collector, and the cursor it returns
--    leaves nothing behind and repeats nothing.
-- ─────────────────────────────────────────────────────────────────────────────
local function drain(after, hdr)
  SE.reset_request({ path = "/v1/admin/usage" })
  SE.env.query = "jwt=" .. JWT .. "&after=" .. after
  SE.env.reqheaders["X-Admin-Token"] = hdr
  SE.run("req", REQ)
  return SE.env.resp
end

do
  eq((drain(0, "wrong") or {}).code, 403, "the drain refuses a bad admin token")

  local r = drain(0, "s3cr3t")
  check(r ~= nil and r.code == 200, "the drain answers an authorised collector")
  local body = r and r.body or ""

  -- Two metered responses so far: one exact completion and one penalty. The
  -- read-only GET was never a completion and the over-budget request never
  -- reached the response phase, so neither left a record.
  -- gmatch is restricted in the sandbox, so count occurrences by hand.
  local n, start = 0, 1
  while true do
    local p = string.find(body, '{"seq":', start, true)
    if not p then break end
    n = n + 1
    start = p + 1
  end
  eq(n, 2, "the drain returns every record appended so far")
  check(string.find(body, '"inst":"', 1, true) ~= nil, "the drain reports the ring instance id")
  check(string.find(body, '"lost":0', 1, true) ~= nil, "nothing was lost")

  -- The cursor the drain handed back must leave the collector with nothing to
  -- re-read: duplicates are how a ledger silently over-bills.
  local np = string.find(body, '"next":', 1, true)
  local nxt = tonumber(string.sub(body, np + 7, string.find(body, ",", np, true) - 1))
  local second = drain(nxt, "s3cr3t")
  check(string.find(second.body, '{"seq":', 1, true) == nil,
    "a second drain at the returned cursor repeats nothing")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. The ring's instance id is stable while the ring lives, so the collector's
--    per-instance cursor stays valid and never rewinds.
-- ─────────────────────────────────────────────────────────────────────────────
do
  local seq = SE.T["ai_useq"].v
  local d = string.find(seq, ":", 1, true)
  local inst_before = string.sub(seq, 1, d - 1)

  -- Step past the window boundary first: the penalty above left this consumer
  -- over budget, and an over-budget request is rejected in HTTP_REQ and never
  -- reaches the response phase that writes records. Crossing the boundary is
  -- also the case worth covering — the budget counter starts a fresh bucket,
  -- but the ledger's ring must NOT reset with it.
  SE.env.now = 1000900
  request({ body = COMPLETION, respheaders = { ["Content-Type"] = "application/json" } })

  local after = SE.T["ai_useq"].v
  local d2 = string.find(after, ":", 1, true)
  eq(string.sub(after, 1, d2 - 1), inst_before, "the ring instance id survives further records")
  check(tonumber(string.sub(after, d2 + 1)) > tonumber(string.sub(seq, d + 1)),
    "the ring head advances")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. The counters endpoint the console already uses is unchanged by all this.
-- ─────────────────────────────────────────────────────────────────────────────
do
  SE.reset_request({ path = "/v1/admin/counters" })
  SE.env.query = "jwt=" .. JWT .. "&users=alice,bob"
  SE.env.reqheaders["X-Admin-Token"] = "s3cr3t"
  SE.run("req", REQ)
  local body = (SE.env.resp or {}).body or ""
  check(string.find(body, '"user":"alice"', 1, true) ~= nil, "counters still reports alice")
  check(string.find(body, '"user":"bob"', 1, true) ~= nil, "counters still reports bob")
end

if failures > 0 then
  io.write(failures, " assertion(s) failed\n")
  os.exit(1)
end
io.write("all SE-stub assertions passed\n")
