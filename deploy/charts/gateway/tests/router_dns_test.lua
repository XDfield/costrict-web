-- Unit tests for dns_utils.lua (parse_resolv_conf / detect_dns).
-- Run with a standard Lua interpreter:
--   cd deploy/charts/gateway
--   LUA_PATH="tests/?.lua;;" lua tests/router_dns_test.lua

local dns = require "dns_utils"

local function eq(a, b)
    if #a ~= #b then return false end
    for i = 1, #a do
        if a[i] ~= b[i] then return false end
    end
    return true
end

local cases = {
    {
        name = "kube-dns standard",
        input = [[# kubelet config
nameserver 10.96.0.10
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = "cluster.local",
    },
    {
        name = "NodeLocal DNS",
        input = [[nameserver 169.254.20.10
search default.svc.cluster.local svc.cluster.local cluster.local
]],
        want_nameservers = {"169.254.20.10"},
        want_domain = "cluster.local",
    },
    {
        name = "multiple nameservers",
        input = [[nameserver 10.96.0.10
nameserver 10.96.0.11
search default.svc.cluster.local svc.cluster.local cluster.local
]],
        want_nameservers = {"10.96.0.10", "10.96.0.11"},
        want_domain = "cluster.local",
    },
    {
        name = "custom cluster domain",
        input = [[nameserver 10.96.0.10
search default.svc.k8s.internal svc.k8s.internal k8s.internal
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = "k8s.internal",
    },
    {
        name = "no matching search domain",
        input = [[nameserver 10.96.0.10
search default.example.com cluster.local.example
]],
        want_nameservers = {"10.96.0.10"},
        want_domain = nil,
    },
    {
        name = "empty file",
        input = "",
        want_nameservers = {},
        want_domain = nil,
    },
}

local passed = 0
for _, c in ipairs(cases) do
    local ns, domain = dns.parse_resolv_conf(c.input)
    local ns_ok = eq(ns, c.want_nameservers)
    local domain_ok = domain == c.want_domain
    if ns_ok and domain_ok then
        passed = passed + 1
        print("PASS: " .. c.name)
    else
        print("FAIL: " .. c.name)
        local got_ns = "{" .. table.concat(ns, ",") .. "}"
        local want_ns = "{" .. table.concat(c.want_nameservers, ",") .. "}"
        print("  got ns=" .. got_ns .. " domain=" .. tostring(domain))
        print("  want ns=" .. want_ns .. " domain=" .. tostring(c.want_domain))
    end
end

-- detect_dns override tests
local ns, domain, source = dns.detect_dns("10.0.0.10", "k8s.internal", "nameserver 10.96.0.10\nsearch default.svc.cluster.local")
if source == "manual" and ns[1] == "10.0.0.10" and domain == "k8s.internal" then
    passed = passed + 1
    print("PASS: detect_dns manual override")
else
    print("FAIL: detect_dns manual override")
    print("  got source=" .. source .. " ns=" .. table.concat(ns, ",") .. " domain=" .. domain)
end

local ns2, domain2, source2 = dns.detect_dns("", "", "nameserver 10.96.0.10\nsearch default.svc.cluster.local")
if source2 == "auto" and ns2[1] == "10.96.0.10" and domain2 == "cluster.local" then
    passed = passed + 1
    print("PASS: detect_dns auto detection")
else
    print("FAIL: detect_dns auto detection")
    print("  got source=" .. source2 .. " ns=" .. table.concat(ns2, ",") .. " domain=" .. domain2)
end

local total = #cases + 2
print(passed .. "/" .. total .. " passed")
os.exit(passed == total and 0 or 1)
