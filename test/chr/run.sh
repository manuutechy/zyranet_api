#!/usr/bin/env bash
# RouterOS lab: proves Zyra Net's real router scripts on a real RouterOS
# (MikroTik CHR, same version as the field routers) before anything ships.
#
#   test/chr/run.sh            # from the zyranet_api repo root
#
# A fresh throwaway router boots in QEMU with NO internet (it can't reach our
# API or anyone else), loads the setup script, the first sync and a repeat
# sync, then test/chr/check.py reads its real state over REST.
# Needs: qemu-system-x86_64, qemu-img, /dev/kvm, curl, python3, go.
# Limits: CHR has no WiFi radio, so the lab router is ether-only.
set -euo pipefail
VER=${CHR_VERSION:-7.25.1}
LAB=${CHR_LAB:-$HOME/zyra-local/chr}
REST=127.0.0.1:18780
REPO=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$LAB/img" "$LAB/serve"
cleanup() { [ -f "$LAB/qemu.pid" ] && kill "$(cat "$LAB/qemu.pid")" 2>/dev/null || true; [ -f "$LAB/serve.pid" ] && kill "$(cat "$LAB/serve.pid")" 2>/dev/null || true; rm -f "$LAB/qemu.pid" "$LAB/serve.pid" "$LAB/run.qcow2"; }
[ "${KEEP:-0}" = 1 ] || trap cleanup EXIT
cleanup

if [ ! -f "$LAB/img/chr-$VER.img" ]; then
  echo "downloading CHR $VER"
  (cd "$LAB/img" && curl -sSfLO "https://download.mikrotik.com/routeros/$VER/chr-$VER.img.zip" && unzip -o -q "chr-$VER.img.zip")
fi

echo "generating scripts"
(cd "$REPO" && go run ./cmd/routertest "$LAB/serve") >/dev/null
cp "$REPO/test/chr/relay.py" "$LAB/relay.py"
printf '#!/bin/sh\nexec python3 -I %s/relay.py\n' "$LAB" > "$LAB/relay.sh"; chmod +x "$LAB/relay.sh"

python3 -m http.server 18781 --bind 127.0.0.1 --directory "$LAB/serve" >"$LAB/serve.log" 2>&1 & echo $! > "$LAB/serve.pid"
qemu-img create -q -f qcow2 -F raw -b "$LAB/img/chr-$VER.img" "$LAB/run.qcow2"
qemu-system-x86_64 -enable-kvm -m 256 -smp 1 -display none -serial "file:$LAB/serial.log" \
  -drive "file=$LAB/run.qcow2,if=virtio,format=qcow2" \
  -netdev "user,id=wan,restrict=on,hostfwd=tcp:$REST-:80,guestfwd=tcp:10.0.2.100:8080-cmd:$LAB/relay.sh" -device virtio-net-pci,netdev=wan \
  -netdev user,id=lan1,restrict=on -device virtio-net-pci,netdev=lan1 \
  -netdev user,id=lan2,restrict=on -device virtio-net-pci,netdev=lan2 >"$LAB/qemu.log" 2>&1 & echo $! > "$LAB/qemu.pid"

for _ in $(seq 1 60); do curl -s -m 3 -o /dev/null -u admin: "http://$REST/rest/system/resource" && break; sleep 2; done
echo "router up: $(curl -s -u admin: "http://$REST/rest/system/resource" | python3 -c 'import json,sys;d=json.load(sys.stdin);print("RouterOS",d["version"])')"

run() { # run a RouterOS script, print its output
  curl -s -m 300 -u admin: -X POST "http://$REST/rest/execute" -H 'content-type: application/json' \
    -d "$(python3 -c 'import json,sys;print(json.dumps({"script":sys.argv[1],"as-string":True}))' "$1")" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("ret",""))'
}
load() { # fetch + import one script; fail on anything but a clean import
  out=$(run "/tool fetch url=http://10.0.2.100:8080/$1 dst-path=$1; /import file-name=$1")
  if ! grep -q "Script file loaded and executed successfully" <<<"$out"; then
    echo "FAIL $1 did not import cleanly:"; tail -5 <<<"$out"; exit 1
  fi
  echo "PASS $1 imports cleanly"
}
logcount() { curl -s -u admin: "http://$REST/rest/log" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))'; }

# MikroTik's factory firewall, as on real routers (RB951 etc.): without it the
# lab once passed while the field router locked the tunnel out (15 Nov 2026).
run '/interface list add name=LAN; /interface list add name=WAN; /interface list member add list=WAN interface=ether1; /ip firewall filter add chain=input action=accept connection-state=established,related,untracked comment="defconf: accept established,related,untracked"; /ip firewall filter add chain=input action=drop connection-state=invalid comment="defconf: drop invalid"; /ip firewall filter add chain=input action=accept protocol=icmp comment="defconf: accept ICMP"; /ip firewall filter add chain=input action=drop in-interface-list=!LAN comment="defconf: drop all not coming from LAN"' >/dev/null
load setup.rsc
load sync1.rsc
n0=$(logcount)
load sync2.rsc
# Config writes the repeat sync made. Device logins changed in the last 10
# minutes are re-sent on purpose (a missed sync can't lose them), so they
# don't count.
changes=$(curl -s -u admin: "http://$REST/rest/log" | python3 -c "
import json,sys,re
L=json.load(sys.stdin)[$n0:]
ch=[e['message'] for e in L if re.search(r' (changed|added|removed) by ', e['message']) and not e['message'].startswith('hotspot user zn')]
for m in ch: print(m[:140], file=sys.stderr)
print(len(ch))")
python3 -I "$REPO/test/chr/check.py" "http://$REST" "$LAB/serve/expect.json" "$changes" || status=1

# Drift: break things the way a person or a reset would, then the next sync
# must put every one of them back (skipping writes must not mean skipping repairs).
echo; echo "breaking settings on purpose, then syncing again"
run ':do { /ip hotspot user profile set [find name="1_Hour"] add-mac-cookie=no rate-limit="1k/1k" }; /ip hotspot set [find name="hs-zyranet"] keepalive-timeout=1m idle-timeout=3m; /ip hotspot profile set [find name="hsp-zyranet"] login-by=http-pap; /ip firewall filter remove [find comment="Zyra Net Block Management"]; /queue tree remove [find name="zq-up-heavy"]; /ip firewall address-list remove [find list="zyranet-mgmt" address="10.200.0.1"]; /ip firewall filter remove [find comment="Zyra Net Remote Access"]; /system script set [find name="zyranet-gc"] source="/ip hotspot host remove [find unauthorized=yes idle-time>5m]"' >/dev/null
load sync2.rsc
python3 -I "$REPO/test/chr/check.py" "http://$REST" "$LAB/serve/expect.json" || status=1
exit "${status:-0}"
