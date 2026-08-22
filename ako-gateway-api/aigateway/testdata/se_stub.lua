-- A stand-in for the Avi Service Engine's Lua DataScript environment.
--
-- The generated scripts only ever run inside the SE, where a mistake costs a
-- 500 on the live front door. This stub reproduces the sandbox faithfully
-- enough to run them here instead — including the ways it is NOT stock Lua,
-- because every one of those has already cost a real outage:
--
--   · tonumber(nil) RAISES rather than returning nil
--   · string.match / string.gmatch are restricted (no Lua patterns)
--   · reading an unknown avi.http field RAISES rather than returning nil, so
--     pcall(avi.http.missing) does NOT protect anything — the field access is
--     evaluated before pcall is called
--   · avi.vs.table_insert is a true insert: it does NOT overwrite a live key
--
-- See docs/gateway-api/ai-gateway-token-ledger.md and the avi-datascript-gotchas
-- notes.

local M = {}

-- The per-VS shared string table, keyed name -> {v=value, exp=absolute expiry}.
M.T = {}

-- Per-request state the scripts read.
M.env = {
  now = 1000000, path = "/v1/chat/completions", query = "", clientip = "10.1.2.3",
  status = 200, body = nil, reqheaders = {}, respheaders = {}, reqvars = {},
  resp = nil, buffered = nil,
  -- What avi.vs.log wrote for this request, in order. This is the client-log
  -- text an operator reads in Analytics > Logs, so a spec can assert on the
  -- thing the demo actually shows rather than on the source that produced it.
  logs = {},
}

function M.reset_request(over)
  M.env.path = "/v1/chat/completions"
  M.env.query = ""
  M.env.status = 200
  M.env.body = nil
  M.env.reqheaders = {}
  M.env.respheaders = {}
  M.env.reqvars = {}
  M.env.resp = nil
  M.env.buffered = nil
  M.env.logs = {}
  for k, v in pairs(over or {}) do M.env[k] = v end
end

-- ── Interleaving (concurrency) support ───────────────────────────────────────

-- A DataScript is not the only thing running on its SE. A VS is served by many
-- dispatcher cores at once and they share this one table, so two responses can
-- be between the same two table calls at the same instant. Nothing in the API
-- makes a read-modify-write atomic, and a single-threaded harness can never see
-- that -- it runs every script to completion before starting the next.
--
-- M.interleave_at makes the schedule explicit instead: the named table op yields
-- the running coroutine, so the spec chooses the interleaving rather than hoping
-- to observe one. Set to nil for ordinary sequential runs.
M.interleave_at = nil

function M.trace(op, key)
  local at = M.interleave_at
  if at and at.op == op and at.key == key and coroutine.isyieldable ~= nil then
    if coroutine.isyieldable() then coroutine.yield(op .. " " .. key) end
  elseif at and at.op == op and at.key == key then
    -- 5.1 has no coroutine.isyieldable; yielding outside a coroutine raises, so
    -- ask the running coroutine instead.
    if coroutine.running() then coroutine.yield(op .. " " .. key) end
  end
end

-- chunk compiles a generated script WITHOUT the pcall wrapper M.run uses. Lua
-- 5.1 cannot yield across a pcall boundary, and the whole point here is to yield
-- from inside the script, so the concurrency spec runs the bare chunk.
function M.chunk(name, src)
  local _c = loadstring or load
  local fn, err = _c(src, name)
  if not fn then error("SYNTAX ERROR in " .. name .. ": " .. tostring(err), 0) end
  return fn
end

-- ── Sandbox fidelity ─────────────────────────────────────────────────────────

-- os.time is the SE clock; the stub drives it so window boundaries and TTLs are
-- deterministic instead of depending on when the test happened to run.
os.time = function() return M.env.now end

local _tonumber = tonumber
function tonumber(x, b)
  if x == nil then
    error("bad argument #1 to 'tonumber' (string expected, got nil)", 2)
  end
  if b == nil then return _tonumber(x) end
  return _tonumber(x, b)
end

string.match = function() error("string.match is restricted in this sandbox", 2) end
string.gmatch = function() error("string.gmatch is restricted in this sandbox", 2) end

-- Any avi.* table raises on an unknown field, which is what makes the bare
-- pcall(avi.http.get_method) form useless: Lua evaluates the argument first, so
-- the raise escapes the pcall entirely.
local function strict(name, t)
  return setmetatable(t, {
    __index = function(_, k) error("no such field: " .. name .. "." .. k, 2) end,
  })
end

-- ── The avi API ──────────────────────────────────────────────────────────────

local env = M.env

local http = strict("avi.http", {
  get_path = function() return env.path end,
  get_query = function(name)
    if name == nil then return env.query end
    local q = "&" .. env.query
    local s = string.find(q, "&" .. name .. "=", 1, true)
    if not s then return nil end
    local rest = string.sub(q, s + #name + 2)
    local amp = string.find(rest, "&", 1, true)
    return amp and string.sub(rest, 1, amp - 1) or rest
  end,
  get_header = function(name, dir)
    if dir == 1 then return env.reqheaders[name] end
    return env.respheaders[name]
  end,
  get_reqvar = function(n) return env.reqvars[n] end,
  set_reqvar = function(n, v) env.reqvars[n] = v end,
  -- Both of these take KILOBYTES, not bytes. That asymmetry against Lua's `#s`
  -- byte count is the whole reason RespBodyBufferBytes exists, and modelling it
  -- correctly here is what makes the truncation assertions mean anything.
  set_response_body_buffer_size = function(kb) env.buffered = kb * 1024 end,
  -- Only what was actually buffered is readable, which is exactly how a body
  -- larger than the buffer loses its trailing `usage` block.
  get_response_body = function(kb)
    if not env.body then return nil end
    local n = kb * 1024
    if env.buffered and env.buffered < n then n = env.buffered end
    return string.sub(env.body, 1, n)
  end,
  status = function() return env.status end,
  response = function(code, hdrs, body) env.resp = { code = code, body = body } end,
  oauth_get_claim = function(_, claim) return { (env.claims or {})[claim] } end,
})

local vs = strict("avi.vs", {
  client_ip = function() return env.clientip end,
  -- Writes a line into this request's client log entry (and marks the entry
  -- significant, which is why a script must not call it unconditionally on a
  -- high-volume VS).
  log = function(msg) env.logs[#env.logs + 1] = tostring(msg) end,
  table_lookup = function(k)
    M.trace("lookup", k)
    local e = M.T[k]
    if e and e.exp > env.now then return e.v end
    return nil
  end,
  -- A true insert. The generated scripts remove first for exactly this reason.
  table_insert = function(k, v, ttl)
    M.trace("insert", k)
    local e = M.T[k]
    if e and e.exp > env.now then return end
    M.T[k] = { v = v, exp = env.now + ttl }
  end,
  table_remove = function(k)
    M.trace("remove", k)
    M.T[k] = nil
  end,
})

local pool = strict("avi.pool", {
  name = function() return "llm-pool" end,
  server_ip = function() return "10.2.3.4" end,
  select = function() end,
})

avi = strict("avi", { http = http, vs = vs, pool = pool, HTTP_REQUEST = 1 })

-- ── Running a generated script ───────────────────────────────────────────────

-- run loads and executes one generated phase script. A syntax error here is the
-- failure mode that would 500 the live front door, so it is reported loudly.
-- loadstring on 5.1, load on 5.2+. The SE runs an older Lua than a dev box, so
-- the harness has to be runnable on both: a script that only compiles under 5.3
-- is not evidence about the SE.
local _compile = loadstring or load

function M.run(name, src)
  local chunk, err = _compile(src, name)
  if not chunk then error("SYNTAX ERROR in " .. name .. ": " .. tostring(err), 0) end
  local ok, rerr = pcall(chunk)
  if not ok then error("RUNTIME ERROR in " .. name .. ": " .. tostring(rerr), 0) end
end

function M.dump_table()
  local keys = {}
  for k in pairs(M.T) do keys[#keys + 1] = k end
  table.sort(keys)
  local out = {}
  for _, k in ipairs(keys) do
    if M.T[k].exp > M.env.now then out[#out + 1] = k .. " = " .. tostring(M.T[k].v) end
  end
  return table.concat(out, "\n")
end

return M
