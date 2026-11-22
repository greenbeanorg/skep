# M7c validation scripts (ADR 0011 Decision 12)

Used on `hearst` on 2026-11-22; results in
[../results/m7c-validation.md](../results/m7c-validation.md).

- `api.py`: a minimal Skep API client that trusts exactly the pinned
  certificate (fetch it first with `node scripts/e2e-trust.mjs fetch HOST:PORT
  PIN hearst-api.pem`). Passwords are read from `~/.skep-test-creds` and never
  printed; bearer tokens are kept in a `0700` directory next to it.
- `cases.sh`: the probe cases (D, A, B, C, T5, E, F, guards G1/G2, control
  frames L1/L2, VLAN V1–V3) against a jail whose port the agent filtered, with
  tcpdump on the attacker's port, the bridge and the victim's port, and an
  automatic verdict per case. Runs on the host; needs `rawl2`/`rawping` in the
  attacker jail and bpf unhidden there for the frame cases.
- `failopen.sh`: breaks a port's filter as host root (rmhook, shutdown,
  pass-all, foreign node on `lower`) and waits for the agent's reaction in its
  audit log.
- `rc.local.m7c`: the forged sender of the ordering test.
