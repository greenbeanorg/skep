#!/usr/bin/env python3
"""Job 4a: two temporary API users on smith for skep-setup's -admin
warning: s4-disabled (role admin, then disabled) and s4-viewer (role
viewer). Passwords are random, sent once, never printed or stored.
Usage: j4a_users.py create|delete"""
import json, secrets, sys
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API

a = API("smith")
users = {"s4-disabled": "admin", "s4-viewer": "viewer"}
if sys.argv[1] == "create":
    for u, role in users.items():
        st, b = a.req("POST", "/users", {"username": u, "password": secrets.token_urlsafe(24), "role": role})
        print(json.dumps({"create": u, "role": role, "http": st, "id": (b or {}).get("id") if isinstance(b, dict) else None}))
        if u == "s4-disabled" and st == 201:
            st2, b2 = a.req("PUT", "/users/" + b["id"], {"disabled": True})
            print(json.dumps({"disable": u, "http": st2, "disabled": (b2 or {}).get("disabled")}))
else:
    st, b = a.req("GET", "/users?limit=200")
    for x in b.get("items", []):
        if x["username"] in users:
            st2, _ = a.req("DELETE", "/users/" + x["id"])
            print(json.dumps({"delete": x["username"], "http": st2}))
    st, b = a.req("GET", "/users?limit=200")
    print(json.dumps({"left": [x["username"] for x in b.get("items", []) if x["username"] in users]}))
