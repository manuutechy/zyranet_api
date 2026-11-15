# RADIUS restart by request file

FreeRADIUS reads its router (NAS) list only when it starts, so switching a
zone to RADIUS (or off) needs a restart. The API used to run
`sudo -n systemctl restart freeradius`; that can never work because the
API's unit has `NoNewPrivileges=yes`. Same trick as `deploy/wg`: the API
writes a request line, root does the checked restart and writes a receipt.

Install on the server (as root, from this folder). Not installed anywhere yet.

```
install -m 755 -o root -g root zyra-radius-reload /usr/local/sbin/zyra-radius-reload
install -d -m 750 -o root -g zyranet /var/lib/zyranet-radius                 # zyranet can't create or rename files here
install -m 660 -o root -g zyranet /dev/null /var/lib/zyranet-radius/reload    # the one file it may write
install -m 644 zyra-radius-reload.path zyra-radius-reload.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now zyra-radius-reload.path
echo 'RADIUS_RELOAD_FILE=/var/lib/zyranet-radius/reload' >> /opt/zyranet-api/.env
systemctl restart zyranet-api
rm -f /etc/sudoers.d/zyranet-radius     # dead: sudo never worked from the API
```

Check it: Platform › RADIUS, switch a test zone on; the message says "the
server restarted". `journalctl -u zyra-radius-reload` shows each run.
By hand: `printf '%s %s\n' $(openssl rand -hex 16) $(date +%s) > /var/lib/zyranet-radius/reload; sleep 5; cat /var/lib/zyranet-radius/reload.done`

What the script refuses: anything but `<32 hex> <unix time>`, requests older
than 2 minutes, symlinks, files over 128 bytes, a FreeRADIUS config that fails
`freeradius -C` (the running server is left alone). At most one restart per 10 s.

Undo: `systemctl disable --now zyra-radius-reload.path`, remove the
`RADIUS_RELOAD_FILE` line, restart the API (it then tells staff to restart
FreeRADIUS by hand).
