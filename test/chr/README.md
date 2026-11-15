# RouterOS lab

Runs Zyra Net's real router scripts on a real RouterOS before they ship.

```
test/chr/run.sh        # from the repo root; ~1 minute
```

- Boots a fresh MikroTik CHR (RouterOS 7.25.1, same as the field routers) in QEMU with **no internet**: it can't reach our API or anyone else.
- `cmd/routertest` writes the setup script and two syncs for a sample router (two plans, paid devices, one plan that just ended, Smart speed and tunnel on), from a throwaway in-memory database.
- Loads setup → sync → repeat sync, then `check.py` reads the router's real state over REST: hotspot, stay-logged-in cookie, plan-ended kick (installed, valid, and run), device logins, Smart speed rules and lanes, tunnel, locked management, and that a repeat sync writes nothing.
- Then breaks settings on purpose and checks the next sync repairs every one.

Needs `qemu-system-x86_64`, `/dev/kvm`, `curl`, `python3`, `go`. `KEEP=1` leaves the router running (REST on 127.0.0.1:18780, user `admin`, no password).
Limit: CHR has no WiFi radio, so WiFi lines aren't exercised here.
