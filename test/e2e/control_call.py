#!/usr/bin/env python3
# Sends one raw JSON request to the lapis control socket and prints the
# JSON response, so tests can forge requests the CLI would never send.
import json, socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect("/run/lapis/control.sock")
s.sendall((sys.argv[1] + "\n").encode())
print(s.makefile().readline().strip())
