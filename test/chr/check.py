#!/usr/bin/env python3
"""Checks a RouterOS lab router after Zyra Net's setup + syncs were imported.

Reads the router's real state over its REST API and compares it with what the
scripts promise (expect.json from cmd/routertest). Prints one line per check
and exits 1 if any fails. Usage: check.py <rest-base-url> <expect.json> [sync2-change-count]
"""
import json, sys, urllib.request, base64

base, expect_path = sys.argv[1].rstrip("/"), sys.argv[2]
sync2_changes = int(sys.argv[3]) if len(sys.argv) > 3 else None
exp = json.load(open(expect_path))
auth = "Basic " + base64.b64encode(b"admin:").decode()

def get(path):
    req = urllib.request.Request(f"{base}/rest/{path}", headers={"Authorization": auth})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)

def run(script):
    body = json.dumps({"script": script, "as-string": True}).encode()
    req = urllib.request.Request(f"{base}/rest/execute", data=body, method="POST",
                                 headers={"Authorization": auth, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r).get("ret", "")

results = []
def check(name, ok, detail=""):
    results.append(ok)
    print(("PASS " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""))

# --- hotspot and stay-logged-in ---
hs = [h for h in get("ip/hotspot") if h["name"] == exp["hotspot_server"]]
check("hotspot server exists and is enabled", len(hs) == 1 and hs[0].get("disabled") == "false", hs)
if hs:
    check("no idle logout, 5 min keepalive", hs[0].get("idle-timeout") == "none" and hs[0].get("keepalive-timeout") == "5m", hs[0])
sp = {p["name"]: p for p in get("ip/hotspot/profile")}
lb = sp.get("hsp-zyranet", {}).get("login-by", "")
check("server profile lets paid phones back in by MAC cookie", "mac-cookie" in lb, lb)

up = {p["name"]: p for p in get("ip/hotspot/user/profile")}
for name in exp["profiles"]:
    p = up.get(name)
    check(f"plan profile {name} exists", p is not None)
    if not p:
        continue
    check(f"{name}: MAC cookie on (3 days)", p.get("add-mac-cookie") == "true" and p.get("mac-cookie-timeout") == "3d", p)
    src = p.get("on-logout", "")
    check(f"{name}: plan-ended kick installed", "registration-table" in src or "hotspot host remove" in src, src[:80])
    parsed = run(f':do {{ :local f [:parse [/ip hotspot user profile get [find name="{name}"] on-logout]]; :put PARSED }} on-error={{ :put PARSE-ERROR }}')
    check(f"{name}: kick script is valid RouterOS", "PARSED" in parsed, parsed[-80:])

# --- device logins ---
users = {u["name"]: u for u in get("ip/hotspot/user")}
for name in exp["active_users"]:
    check(f"paid login {name} on the router", name in users)
u2 = users.get("znlabuser2", {})
check("data-capped login carries its data limit", u2.get("limit-bytes-total") == str(exp["data_limit_b"]), u2.get("limit-bytes-total"))
for name in exp["removed_users"]:
    check(f"ended login {name} removed", name not in users)

# --- the kick really runs: a fake 'used up' logout for an existing user ---
out = run(r'''
:local src [/ip hotspot user profile get [find name="1_Hour"] on-logout]
:local f [:parse (":local user \"gone-user\"; :local address \"10.5.50.77\"; :local \"mac-address\" \"02:00:00:00:00:77\"; " . $src)]
/ip hotspot host print count-only
$f
:put KICK-RAN
''')
check("kick script runs without error for a used-up login", "KICK-RAN" in out, out[-120:])

# --- smart speed ---
# Freshly (re)built rules show "invalid" for a few seconds while RouterOS
# re-checks them; give it up to 15 s to settle before judging.
import time
for _ in range(15):
    if not any(x.get("invalid") == "true" for x in get("ip/firewall/mangle") + get("queue/tree")):
        break
    time.sleep(1)
mangle = [m for m in get("ip/firewall/mangle") if m.get("comment", "").startswith("zyra-smart")]
check("smart speed: 7 traffic rules", len(mangle) == 7, len(mangle))
order = [m.get("new-packet-mark") for m in mangle]
check("smart speed: catch-all (big downloads) is last", order[-1:] == ["zq-heavy"], order)
check("smart speed: rules are active", all(m.get("disabled") != "true" and m.get("invalid") != "true" for m in mangle),
      [m for m in mangle if m.get("invalid") == "true"][:1])
qt = {q["name"]: q for q in get("queue/tree")}
want_q = ["zq-down", "zq-up"] + [f"zq-{d}-{l}" for d in ("down", "up") for l in ("rt", "quick", "heavy")]
check("smart speed: 8 lanes on the router", all(q in qt for q in want_q), [q for q in want_q if q not in qt])
if "zq-down" in qt:
    check("smart speed: download capped at 92% of the line", qt["zq-down"].get("max-limit") == str(exp["down_kbps"] * 1000), qt["zq-down"].get("max-limit"))
    check("smart speed: download lanes on the customer bridge", qt["zq-down"].get("parent") == "bridge-hotspot", qt["zq-down"].get("parent"))
if "zq-up" in qt:
    check("smart speed: upload on the internet port", qt["zq-up"].get("parent") == "ether1", qt["zq-up"].get("parent"))
if "zq-down-rt" in qt:
    check("smart speed: games and calls first", qt["zq-down-rt"].get("priority") == "1" and qt.get("zq-down-heavy", {}).get("priority") == "7")
check("smart speed: no queue reported invalid", not any(q.get("invalid") == "true" for q in qt.values()), [n for n, q in qt.items() if q.get("invalid") == "true"])
ff = [r for r in get("ip/firewall/filter") if r.get("action") == "fasttrack-connection" and r.get("disabled") == "false"]
check("no fasttrack rule skipping the lanes", not ff, ff[:1])

# --- tunnel ---
wg = [w for w in get("interface/wireguard") if w["name"] == "zyra-wg"]
check("tunnel interface zyra-wg exists", len(wg) == 1)
addr = [a for a in get("ip/address") if a.get("interface") == "zyra-wg"]
check("tunnel address set", any(a["address"] == exp["tunnel_ip"] + "/24" for a in addr), addr)
peers = [p for p in get("interface/wireguard/peers") if p.get("interface") == "zyra-wg"]
check("server is the tunnel peer (keepalive 25s)", len(peers) == 1 and peers[0].get("persistent-keepalive") == "25s", peers)
api_user = [u for u in get("user") if u["name"] == exp["api_user"]]
check("server's API login, tunnel only", len(api_user) == 1 and api_user[0].get("address", "").startswith("10.200.0.1"), api_user)

# --- management stays locked ---
svc = {s["name"]: s for s in get("ip/service")}
check("API service limited to the server", "10.200.0.1" in svc.get("api", {}).get("address", svc.get("api", {}).get("available-from", "")))

# --- idempotency: the second sync must not rebuild things ---
if sync2_changes is not None:
    check("second sync changes nothing that matters", sync2_changes == 0, f"{sync2_changes} config changes")

passed = sum(results)
print(f"\n{passed}/{len(results)} checks passed")
sys.exit(0 if passed == len(results) else 1)
