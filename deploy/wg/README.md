# Tunnel hub: automatic router peers

The API writes `/var/lib/zyranet-wg/peers`; root applies it to `wg0`.

```
install -m 755 -o root -g root zyra-wg-apply /usr/local/sbin/zyra-wg-apply
install -d -m 750 -o root -g zyranet /var/lib/zyranet-wg          # zyranet can't create or rename files here
install -m 640 -o zyranet -g zyranet /dev/null /var/lib/zyranet-wg/peers   # the one file it may write
install -m 644 -o root -g root /dev/null /var/lib/zyranet-wg/peers.applied
install -m 644 zyra-wg-peers.path zyra-wg-apply.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now zyra-wg-peers.path
echo 'PostUp = /usr/local/sbin/zyra-wg-apply || true' >> /etc/wireguard/wg0.conf   # peers back after a reboot
echo 'WG_PEER_FILE=/var/lib/zyranet-wg/peers' >> /opt/zyranet-api/.env && systemctl restart zyranet-api
```

Undo: `systemctl disable --now zyra-wg-peers.path`, remove the `WG_PEER_FILE` line, restart the API.
