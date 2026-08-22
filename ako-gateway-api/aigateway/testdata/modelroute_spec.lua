-- Runs the generated model-route DataScripts in the SE sandbox and asserts on
-- the two things the request actually leaves behind: the Pool Group it selected
-- and the line it wrote into the VS client log.
--
-- The log line is not decoration. Under EVH the pool and pool-group names Avi
-- shows in Analytics > Logs are `<prefix>--<sha1>`, so without it the client log
-- can show that two requests reached different backends but never that one
-- asked for a premium model and was downgraded. Anything an operator reads off
-- the console during an incident is worth a test.
--
-- Usage: lua modelroute_spec.lua <dir containing se_stub.lua, mrreq.lua, mrreqdata.lua>

local dir = arg[1] or "."
local SE = dofile(dir .. "/se_stub.lua")

local function slurp(n)
  local f = assert(io.open(dir .. "/" .. n, "rb"))
  local s = f:read("*a")
  f:close()
  return s
end

-- ── Request-phase stubs ──────────────────────────────────────────────────────
--
-- se_stub.lua models the response side (that is where the token ledger lives).
-- The model-route scripts run on the request side, so the few request APIs they
-- use are added here rather than in the shared stub, which a parallel branch is
-- editing. Fold them into se_stub.lua once these two land together.
--
-- avi.* tables raise on an unknown field but define no __newindex, so extending
-- them from outside is exactly how the SE's own build variants differ.
local selected_pg = nil
avi.poolgroup = { select = function(pg) selected_pg = pg end }
avi.http.set_request_body_buffer_size = function(_) end
-- The SE hands back only what it buffered, and the model field sits at the head
-- of the JSON, which is the whole reason a 32 KB buffer is enough.
avi.http.get_req_body = function(n) return string.sub(SE.env.reqbody or "", 1, n) end
avi.http.set_path = function(p) SE.env.path = p end
avi.http.set_query = function(q) SE.env.query = q end
avi.http.replace_header = function(n, v) SE.env.reqheaders[n] = v end
avi.http.add_header = function(n, v) SE.env.reqheaders[n] = v end
avi.http.remove_header = function(n) SE.env.reqheaders[n] = nil end

local REQ, REQDATA = slurp("mrreq.lua"), slurp("mrreqdata.lua")

-- ── Harness ──────────────────────────────────────────────────────────────────

local failures = 0
local function check(what, got, want)
  if got ~= want then
    failures = failures + 1
    io.write(string.format("FAIL %s\n  got  %s\n  want %s\n", what, tostring(got), tostring(want)))
  else
    io.write(string.format("ok   %s = %s\n", what, tostring(got)))
  end
end

-- route sends one chat completion through both phases and returns what the SE
-- would have recorded: the Pool Group, the log line, and any early response.
local function route(model, jwt)
  SE.reset_request({ reqbody = '{"model":"' .. model .. '","messages":[{"role":"user","content":"hi"}]}' })
  SE.env.query = jwt and ("jwt=hdr." .. jwt .. ".sig") or ""
  selected_pg = nil
  SE.run("mrreq", REQ)
  SE.run("mrreqdata", REQDATA)
  return selected_pg, SE.env.logs[1], SE.env.resp
end

-- Payloads are the JWT middle segment only: the SE has already validated the
-- signature by the time a DataScript reads a claim.
local PLATINUM = "eyJzdWIiOiJib2IiLCJncm91cCI6InBsYXRpbnVtIn0"     -- premium+standard+economy
local GOLD = "eyJzdWIiOiJhbGljZSIsImdyb3VwIjoiZ29sZCJ9"            -- standard+economy
local NOBODY = "eyJzdWIiOiJuZW1vIiwiZ3JvdXAiOiJub2JvZHkifQ"        -- no entitlement at all

-- 1. Exact model match, caller entitled: routed to premium and the log says so.
local pg, log = route("llama-3-70b-instruct", PLATINUM)
check("premium pool group", pg, "vs-tier-premium-pg")
check("premium log line", log,
  "ai-gateway: model=llama-3-70b-instruct tier=premium pool-group=vs-tier-premium-pg")

-- 2. The demo's best beat: same request, weaker group. The pool group changes
--    silently; only the log says the caller asked for premium and got standard.
pg, log = route("llama-3-70b-instruct", GOLD)
check("downgraded pool group", pg, "vs-tier-standard-pg")
check("downgrade named in the log", log,
  "ai-gateway: model=llama-3-70b-instruct tier=standard requested=premium downgraded=1"
  .. " pool-group=vs-tier-standard-pg")

-- 3. Longest-prefix glob wins, and the log reports the model as sent.
pg, log = route("mistral-7b-instruct-v3", PLATINUM)
check("longest-prefix pool group", pg, "vs-tier-premium-pg")
check("longest-prefix log line", log,
  "ai-gateway: model=mistral-7b-instruct-v3 tier=premium pool-group=vs-tier-premium-pg")

-- 4. Unknown model falls to the default tier. Worth logging precisely because
--    nothing else distinguishes it from a deliberate economy request.
pg, log = route("who-knows", PLATINUM)
check("default tier pool group", pg, "vs-tier-economy-pg")
check("default tier log line", log,
  "ai-gateway: model=who-knows tier=economy pool-group=vs-tier-economy-pg")

-- 5. A body with no model at all must not produce a nil-concatenation raise —
--    that is a 500 on the front door, and an empty field in a log line is not.
SE.reset_request({ reqbody = '{"messages":[]}' })
SE.env.query = "jwt=hdr." .. PLATINUM .. ".sig"
selected_pg = nil
SE.run("mrreq", REQ)
SE.run("mrreqdata", REQDATA)
check("bodyless model routes to default", selected_pg, "vs-tier-economy-pg")
check("bodyless model log line", SE.env.logs[1],
  "ai-gateway: model=- tier=economy pool-group=vs-tier-economy-pg")

-- 6. A caller with no entitlement at all is rejected, and the rejection happens
--    BEFORE the log line: a request that never routed must not claim a tier.
local _, log6, resp = route("llama-3-70b-instruct", NOBODY)
check("unentitled group rejected", resp and resp.code, 403)
check("no routing log for a rejected request", log6, nil)

-- 7. Exactly one log line per request. avi.vs.log marks the entry significant,
--    so a second call is a second entry and doubles the log volume of the VS.
route("llama-3-8b-instruct", PLATINUM)
check("one log line per request", #SE.env.logs, 1)

if failures > 0 then
  error(string.format("%d model-route assertion(s) failed", failures), 0)
end
io.write("model-route scripts ran in the SE sandbox; log line and pool group verified\n")
