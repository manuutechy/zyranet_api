# QEMU guestfwd helper: one run per guest connection, joins stdin/stdout to
# the lab's file server on 127.0.0.1:18781.
import os, selectors, socket, sys
s = socket.create_connection(("127.0.0.1", 18781))
sel = selectors.DefaultSelector()
sel.register(0, selectors.EVENT_READ)
sel.register(s, selectors.EVENT_READ)
while True:
    for key, _ in sel.select():
        if key.fileobj == 0:
            d = os.read(0, 65536)
            if not d:
                s.shutdown(socket.SHUT_WR); sel.unregister(0); continue
            s.sendall(d)
        else:
            d = s.recv(65536)
            if not d:
                sys.exit(0)
            os.write(1, d)
