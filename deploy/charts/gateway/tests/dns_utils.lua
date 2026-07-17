-- Pure-Lua DNS configuration helpers for nginx-router.
-- This module intentionally does NOT depend on ngx/OpenResty APIs so it can be
-- unit-tested with a standard Lua interpreter.

local _M = {}

-- Parse /etc/resolv.conf into nameserver list and cluster domain.
-- Returns: nameservers (array of strings), cluster_domain (string or nil)
function _M.parse_resolv_conf(text)
    if not text or text == "" then
        return {}, nil
    end

    local nameservers = {}
    local cluster_domain = nil

    for line in text:gmatch("[^\r\n]+") do
        -- strip comments
        local comment_pos = line:find("[%#;]")
        if comment_pos then
            line = line:sub(1, comment_pos - 1)
        end
        line = line:match("^%s*(.-)%s*$")
        if line ~= "" then
            local ns = line:match("^nameserver%s+(%S+)$")
            if ns then
                nameservers[#nameservers + 1] = ns
            else
                local search_list = line:match("^search%s+(.+)$")
                if search_list and not cluster_domain then
                    for part in search_list:gmatch("%S+") do
                        local domain = part:match("^[^%.]+%.svc%.(.+)$")
                        if domain then
                            cluster_domain = domain
                            break
                        end
                    end
                end
            end
        end
    end

    return nameservers, cluster_domain
end

-- Detect effective DNS configuration for this Pod.
-- Parameters are the raw Helm-rendered overrides (empty string means "not set").
-- resolv_conf_text is optional; nil means read /etc/resolv.conf from disk.
-- Returns: nameservers (array), cluster_domain (string), source (string)
function _M.detect_dns(resolver_override, domain_override, resolv_conf_text)
    local source = "manual"
    local nameservers = {}
    local cluster_domain = nil

    if resolver_override and resolver_override ~= "" then
        for part in resolver_override:gmatch("%S+") do
            nameservers[#nameservers + 1] = part
        end
    end

    if domain_override and domain_override ~= "" then
        cluster_domain = domain_override
    end

    if #nameservers == 0 or not cluster_domain then
        local text = resolv_conf_text
        if not text then
            local f = io.open("/etc/resolv.conf", "r")
            if f then
                text = f:read("*a")
                f:close()
            end
        end

        if text then
            local auto_ns, auto_domain = _M.parse_resolv_conf(text)
            if #nameservers == 0 and #auto_ns > 0 then
                nameservers = auto_ns
                source = "auto"
            end
            if not cluster_domain and auto_domain then
                cluster_domain = auto_domain
                if source == "manual" then source = "auto" end
            end
        end
    end

    if #nameservers == 0 then
        nameservers = {"kube-dns.kube-system.svc." .. (cluster_domain or "cluster.local")}
        source = "fallback"
    end

    if not cluster_domain then
        cluster_domain = "cluster.local"
        if source == "auto" then source = "fallback" end
    end

    return nameservers, cluster_domain, source
end

return _M
