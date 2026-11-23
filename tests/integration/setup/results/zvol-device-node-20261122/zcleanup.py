import sys, json, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from zapi import API
name = sys.argv[1]; a = API(name)
st, vms = a.req("GET", "/vms?limit=100")
items = vms.get("items", vms) if isinstance(vms, dict) else vms
test = [v for v in items if v["name"].startswith(("zc-", "zk-", "zgpt", "zt-"))]
order = sorted(test, key=lambda v: (0 if v["name"].startswith("zk-") else 2 if v["name"] == "zgpt" else 1, v["name"]))
bad = 0
for v in order:
    if v.get("observed", {}).get("state") not in ("stopped", None):
        st, b = a.req("POST", f"/vms/{v['id']}/actions", {"action": "force_stop"}); a.task(st, b, "fs")
    st, b = a.req("DELETE", f"/vms/{v['id']}")
    t = a.task(st, b, "delete " + v["name"])
    if t["status"] != "succeeded":
        bad += 1; print("FAILED", v["name"], t.get("error"))
st, vms = a.req("GET", "/vms?limit=100")
left = vms.get("items", vms) if isinstance(vms, dict) else vms
print(name, "deleted", len(order) - bad, "failed", bad, "vms left", len(left))
