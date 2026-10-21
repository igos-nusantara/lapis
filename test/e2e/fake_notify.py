#!/usr/bin/env python3
# Minimal org.freedesktop.Notifications server for e2e tests: logs every
# Notify call as JSON lines to the file given as argv[1].
import json, sys
import dbus, dbus.service, dbus.mainloop.glib
from gi.repository import GLib

LOG = sys.argv[1]

class Server(dbus.service.Object):
    next_id = 1

    @dbus.service.method("org.freedesktop.Notifications", in_signature="susssasa{sv}i", out_signature="u")
    def Notify(self, app, replaces, icon, summary, body, actions, hints, timeout):
        nid = Server.next_id
        Server.next_id += 1
        with open(LOG, "a") as f:
            f.write(json.dumps({"id": nid, "app": str(app), "summary": str(summary), "body": str(body),
                                "entry": str(hints.get("desktop-entry", "")), "image": "image-data" in hints}) + "\n")
        return nid

    @dbus.service.method("org.freedesktop.Notifications", in_signature="u")
    def CloseNotification(self, nid):
        pass

    @dbus.service.method("org.freedesktop.Notifications", out_signature="as")
    def GetCapabilities(self):
        return ["body", "actions"]

    @dbus.service.method("org.freedesktop.Notifications", out_signature="ssss")
    def GetServerInformation(self):
        return ("fake", "lapis-e2e", "1", "1.2")

dbus.mainloop.glib.DBusGMainLoop(set_as_default=True)
name = dbus.service.BusName("org.freedesktop.Notifications", dbus.SessionBus())
Server(dbus.SessionBus(), "/org/freedesktop/Notifications")
GLib.MainLoop().run()
