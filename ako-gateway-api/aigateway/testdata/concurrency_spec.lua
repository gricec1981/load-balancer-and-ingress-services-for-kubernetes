-- What one SE does when two responses complete at the same instant.
--
-- Every other test here runs one script to completion before starting the next,
-- so it can only ever observe the single-threaded case. A VS is not served that
-- way: many dispatcher cores share one VS table, and nothing in the DataScript
-- API makes a read-modify-write atomic. Both the budget counter and the ledger's
-- ring head are read-modify-writes:
--
--   local cur = avi.vs.table_lookup(k)   -- read
--   avi.vs.table_remove(k)               -- ... another response can be HERE ...
--   avi.vs.table_insert(k, cur + n)      -- write
--
-- The remove is there because table_insert will not overwrite a live key -- but
-- removing first is exactly what gives up that protection.
--
-- So this spec does not hope to catch a race; it schedules one. Each response
-- runs as a coroutine and the stub yields it at a named table op, which makes
-- the damaging interleaving deterministic and the measurement exact.

local dir = arg[1] or error("usage: concurrency_spec.lua <dir-with-generated-scripts>")
local SE = dofile(dir .. "/se_stub.lua")

local function slurp(name)
  local f = assert(io.open(dir .. "/" .. name, "rb"))
  local s = f:read("a")
  f:close()
  return s
end

local REQ, RESP, RESPDATA = slurp("req.lua"), slurp("resp.lua"), slurp("respdata.lua")

local failures = 0
local function check(ok, msg, got, want)
  if ok then return end
  failures = failures + 1
  io.write("FAIL: ", msg)
  if want ~= nil then io.write("\n      got:  ", tostring(got), "\n      want: ", tostring(want)) end
  io.write("\n")
end
local function eq(got, want, msg) check(got == want, msg, got, want) end

-- Fixtures ------------------------------------------------------------------

local JWT = "hdr.eyJzdWIiOiJhbGljZSIsImdyb3VwIjoiZ3JvdXAxIn0.sig"
local TOKENS = 48 -- total_tokens in COMPLETION below

local COMPLETION = '{"id":"chatcmpl-1","object":"chat.completion","created":1,' ..
  '"model":"qwen3-14b-awq","choices":[{"index":0,"message":{"role":"assistant",' ..
  '"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,' ..
  '"completion_tokens":17,"total_tokens":48,' ..
  '"prompt_tokens_details":{"cached_tokens":12},' ..
  '"completion_tokens_details":{"reasoning_tokens":5}}}'

local function counter_key()
  return "7:hourly-group-budget:alice:" .. (math.floor(SE.env.now / 3600) * 3600)
end
local function counter()
  local e = SE.T[counter_key()]
  return e and tonumber(e.v) or 0
end
local function record_count()
  local n = 0
  for k, e in pairs(SE.T) do
    if string.sub(k, 1, 8) == "ai_urec:" and e.exp > SE.env.now then n = n + 1 end
  end
  return n
end
local function head()
  local cur = SE.T["ai_useq"]
  if not cur then return 0 end
  local d = string.find(cur.v, ":", 1, true)
  return tonumber(string.sub(cur.v, d + 1))
end

-- Put the request state in place and run the phases that precede the response
-- body. Only HTTP_RESP_DATA touches the counter and the ring, so that is the
-- phase the two responses overlap in.
local function arrive()
  SE.reset_request({ body = COMPLETION, respheaders = { ["Content-Type"] = "application/json" } })
  SE.env.query = "jwt=" .. JWT
  SE.run("req", REQ)
  SE.run("resp", RESP)
end

-- Run two responses through HTTP_RESP_DATA overlapped: both are stopped at `at`
-- before either has written, then both are released. This is the ordinary
-- lost-update schedule, not an exotic one -- it is what two cores completing
-- within a few microseconds of each other produce.
local function overlap(at)
  arrive()
  local a = coroutine.create(SE.chunk("respdata-a", RESPDATA))
  local b = coroutine.create(SE.chunk("respdata-b", RESPDATA))

  SE.interleave_at = at
  local oka, ya = coroutine.resume(a)
  check(oka, "response A ran to the interleave point", ya)
  check(ya ~= nil, "response A reached " .. at.op .. " " .. at.key, "never reached it")
  local okb, yb = coroutine.resume(b)
  check(okb, "response B ran to the interleave point", yb)
  check(yb ~= nil, "response B reached " .. at.op .. " " .. at.key, "never reached it")

  SE.interleave_at = nil -- release both; neither yields again
  local oka2, ea2 = coroutine.resume(a)
  check(oka2, "response A completed", ea2)
  local okb2, eb2 = coroutine.resume(b)
  check(okb2, "response B completed", eb2)
end

-- The same two responses with no overlap at all. This is the control: if the
-- sequential case disagrees with the overlapped one, the difference is the
-- interleaving and nothing else.
local function sequential()
  arrive(); SE.run("respdata", RESPDATA)
  arrive(); SE.run("respdata", RESPDATA)
end

-- 1. Control: two responses, one after the other. ----------------------------
do
  SE.T = {}
  sequential()
  eq(counter(), 2 * TOKENS, "control: two sequential responses charge both")
  eq(record_count(), 2, "control: two sequential responses leave two records")
  eq(head(), 2, "control: the ring head counts both")
end

-- 2. The budget counter, overlapped. -----------------------------------------
--
--    This is the enforcement-relevant one. An undercount here is not a reporting
--    error: it is quota a consumer gets for free, and it grows with concurrency
--    -- exactly the condition a budget is meant to hold under.
do
  SE.T = {}
  overlap({ op = "remove", key = counter_key() })
  eq(counter(), 2 * TOKENS, "two overlapped responses charge the budget for both")
end

-- 3. The ledger ring, overlapped. --------------------------------------------
--
--    Both responses claim the same sequence number. The record block removes the
--    record key before inserting, so the second write overwrites the first: one
--    response is billed to a consumer and then vanishes from the ledger. The
--    collector cannot detect it -- the sequence has no gap.
do
  SE.T = {}
  overlap({ op = "remove", key = "ai_useq" })
  eq(record_count(), 2, "two overlapped responses leave two usage records")
  eq(head(), 2, "the ring head accounts for both responses")
end

-- 4. Conservation at width: N responses all overlapped at the ring head. -----
--
--    Two is enough to prove the race exists; this says what it costs. Under real
--    load the loss is not a rounding error, and the ledger reports the survivors
--    as if they were everything.
do
  local N = 8
  SE.T = {}
  arrive()
  local co = {}
  SE.interleave_at = { op = "remove", key = "ai_useq" }
  for i = 1, N do
    co[i] = coroutine.create(SE.chunk("respdata-" .. i, RESPDATA))
    coroutine.resume(co[i])
  end
  SE.interleave_at = nil
  for i = 1, N do coroutine.resume(co[i]) end

  io.write(string.format(
    "  [conservation] %d concurrent responses -> %d record(s), head %d, %d of %d tokens charged\n",
    N, record_count(), head(), counter(), N * TOKENS))
  eq(record_count(), N, "every concurrent response is recorded exactly once")
end

if failures > 0 then
  io.write(failures, " assertion(s) failed\n")
  os.exit(1)
end
io.write("all SE-concurrency assertions passed\n")
