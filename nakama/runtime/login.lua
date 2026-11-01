-- login.lua -- Nakama runtime module: exchange a Nakama session for a backend player.
--
-- Why Lua rather than the Go plugin (see ADR 0009): a Go plugin must be built with
-- the same toolchain, the same nakama-common version AND the same build settings as
-- the hosting binary, because plugin.Open compares package hashes. The Nakama image
-- is built with -asmflags="-trimpath /go/build/nakama", which no plugin built
-- elsewhere can reproduce. This bridge is about thirty lines; a Lua module has no
-- ABI at all and cannot crash the server process.
--
-- Contract (unchanged from the Go plugin):
--   client --(Nakama session)--> RPC "login" --> backend POST /v1/account/sessions
--   returns the backend JSON verbatim: {"player": {...}, "session_token": "..."}
local nk = require("nakama")

-- Compose service name by default; override with the runtime env var
-- backend_base_url (Nakama --runtime.env / runtime.env in config).
local DEFAULT_BACKEND = "http://backend:8080"
local DEFAULT_SERVICE_TOKEN = "dev-service"

local function settings(ctx)
    local base, token = DEFAULT_BACKEND, DEFAULT_SERVICE_TOKEN
    local env = ctx and ctx.env
    if env then
        if type(env.backend_base_url) == "string" and env.backend_base_url ~= "" then
            base = env.backend_base_url
        end
        if type(env.backend_service_token) == "string" and env.backend_service_token ~= "" then
            token = env.backend_service_token
        end
    end
    return base, token
end

local function rpc_login(ctx, payload)
    local user_id = ctx and ctx.user_id or ""
    if user_id == "" then
        -- gRPC UNAUTHENTICATED (HTTP 401)
        error({ "login: no Nakama user in context", 16 })
    end

    local base, token = settings(ctx)
    local body = nk.json_encode({
        external_id = user_id,
        display_name = ctx.username or "",
    })
    local headers = {
        ["Content-Type"] = "application/json",
        ["Authorization"] = "Bearer " .. token,
    }

    local url = base .. "/v1/account/sessions"
    -- Nakama's Lua http_request reports an argument-order mismatch as a plain Lua
    -- argument error, and the order has differed between releases (3.22 wants headers
    -- as argument #3). Try the documented order, then the one 3.22 accepts; both are
    -- logged so the next reader knows which one the image uses.
    local first_err
    local ok, code, resp = pcall(nk.http_request, url, "POST", headers, body, 5000)
    if ok then
        nk.logger_info("login: http_request(headers, body) form accepted")
    else
        first_err = tostring(code)
        ok, code, resp = pcall(nk.http_request, url, "POST", body, headers, 5000)
        if ok then
            nk.logger_info("login: http_request(body, headers) form accepted")
        end
    end
    if not ok then
        local detail = string.format("backend call failed (%s): %s | %s",
            url, first_err or "-", tostring(code))
        nk.logger_error("login: " .. detail)
        error({ "login: " .. detail, 14 })
    end
    if code ~= 200 then
        nk.logger_error(string.format("login: backend returned %s: %s", tostring(code), tostring(resp)))
        error({ "login: backend rejected the exchange", 14 })
    end
    if type(resp) ~= "string" or resp == "" then
        nk.logger_error("login: backend returned an empty body")
        error({ "login: backend returned nothing", 13 })
    end

    return resp
end

nk.register_rpc(rpc_login, "login")
nk.logger_info("gamemodule loaded: rpc=login (lua)")