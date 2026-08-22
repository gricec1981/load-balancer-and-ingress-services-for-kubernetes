-- Runs the generated model-route DataScripts for a policy that has a REMOTE
-- tier -- a peer AI Gateway at another site -- in the SE sandbox.
--
-- A remote tier is the one routing decision that leaves the site, so the two
-- things worth proving are what it changes and what it must not change:
--
--   * it selects the peer's Pool Group and rewrites Host, so the peer's EVH
--     child VS matches instead of 404-ing on our hostname; and
--   * it changes NOTHING else. In particular it must not set ai_skip_meter the
--     way a provider tier does. Forwarding a request across a site boundary and
--     silently ceasing to count its tokens would make a cross-site budget a
--     fiction, and the failure is invisible -- the request succeeds either way.
--
-- Usage: lua modelroute_remote_spec.lua <dir containing se_stub.lua, mrreq.lua, mrreqdata.lua>

local dir = arg[1] or "."
local SE = dofile(dir .. "/se_stub.lua")

local function slurp(n)
  local f = assert(io.open(dir .. "/" .. n, "rb"))
  local s = f:read("*a")
  f:close()
  return s
end

-- ── Request-phase stubs (see modelroute_spec.lua) ────────────────────────────
local selected_pg = nil
avi.poolgroup = { select = function(pg) selected_pg = pg end }
avi.http.set_request_body_buffer_size = function(_) end
avi.http.get_req_body = function(n) return string.sub(SE.env.reqbody or "", 1, n) end
avi.http.set_path = function(p) SE.env.path = p end
avi.http.set_query = function(q) SE.env.query = q end
avi.http.replace_header = function(n, v) SE.env.reqheaders[n] = v end
avi.http.add_header = function(n, v) SE.env.reqheaders[n] = v end
avi.http.remove_header = function(n) SE.env.reqheaders[n] = nil end

local REQ, REQDATA = slurp("mrreq.lua"), slurp("mrreqdata.lua")

local failures = 0
local function check(what, got, want)
  if got ~= want then
    failures = failures + 1
    io.write(string.format("FAIL %s\n  got  %s\n  want %s\n", what, tostring(got), tostring(want)))
  else
    io.write(string.format("ok   %s = %s\n", what, tostring(got)))
  end
end

-- The client always arrives at OUR front door, so the inbound Host is ours.
local LOCAL_HOST = "llm.ai.avi.com"
local PEER_HOST = "llm.siteb.ai.avi.com"

local function route(model, jwt)
  SE.reset_request({ reqbody = '{"model":"' .. model .. '","messages":[{"role":"user","content":"hi"}]}' })
  SE.env.reqheaders["Host"] = LOCAL_HOST
  SE.env.query = jwt and ("jwt=hdr." .. jwt .. ".sig") or ""
  selected_pg = nil
  SE.run("mrreq", REQ)
  SE.run("mrreqdata", REQDATA)
  return selected_pg, SE.env.reqheaders["Host"], SE.env.reqvars
end

local PLATINUM = "eyJzdWIiOiJib2IiLCJncm91cCI6InBsYXRpbnVtIn0" -- entitled to every tier

-- 1. A model mapped to the remote tier crosses the boundary: peer Pool Group,
--    Host rewritten to the peer so its child VS matches.
local pg, host, vars = route("llama-3-70b-instruct-eu", PLATINUM)
check("remote pool group", pg, "ns-pol-premium-eu-remote-pg")
check("Host rewritten to the peer", host, PEER_HOST)

-- 2. The tier still reaches the token policy. ai_tier is what the per-tier
--    budget script reads, and ai_skip_meter is what would switch metering off.
check("remote tier still tagged for metering", vars["ai_tier"], "premium-eu")
check("remote tier does NOT skip metering", vars["ai_skip_meter"], nil)

-- 3. The path is untouched: a peer speaks our dialect, unlike a provider.
check("path not rewritten for a peer", SE.env.path, "/v1/chat/completions")

-- 4. A local tier in the SAME policy is unaffected -- the Host rewrite is keyed
--    on the tier, so adding a remote tier must not disturb local traffic.
pg, host = route("llama-3-70b-instruct", PLATINUM)
check("local pool group unchanged", pg, "vs-tier-premium-pg")
check("local tier keeps the client Host", host, LOCAL_HOST)

-- 5. The default tier is local here, so an unknown model stays local too.
pg, host = route("who-knows", PLATINUM)
check("unknown model stays on the default local tier", pg, "vs-tier-economy-pg")
check("default tier keeps the client Host", host, LOCAL_HOST)

if failures > 0 then
  error(string.format("%d remote-tier assertion(s) failed", failures), 0)
end
io.write("remote-tier scripts ran in the SE sandbox; peer pool group, Host rewrite and metering verified\n")
