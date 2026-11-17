# Step 7 build: progress log

ADR 0010 phase 7b, step 7 (the hostops Jails interface, the jail.conf
generator, the baseline, the openat writer; container.create, start,
stop, restart, delete: Decisions 8, 9, 13), branch `feat/m7b-templates`.
Started 2026-11-16 from `649524e` (one commit past `3da430a`: the record
of the disabled step 6 test users; no code). hearst runs `a34749d`.

## 1. Feasibility probe on hearst: STOPPED (spec not buildable as written)

**What was done.** Before writing code, I hand-ran the specification on
hearst: a clone of `minimal-151` in a new dataset `skeptest/step7-probe`
(0700 directory), and a jail started by jail(8) from a generated
jail.conf with Decision 9's baseline and `/etc/rc`. Two variants of the
`allow.*` set. Record: [step7-probe.txt](step7-probe.txt); script:
[../step7-probe/probe-jail.sh](../step7-probe/probe-jail.sh).

**Held as specified:** the clone of the read-only template is writable
(`readonly=off`, inherited default; the template stays `readonly=on`);
`/etc/rc` starts cleanly (lo0, syslogd, cron; only rc.d/routing's usual
"route already in table" lines for lo0); securelevel 2, enforce_statfs 2,
children.max 0, devfs ruleset 5 loaded and applied; mount, kldload,
sysctl writes and sethostname refused.

**Found:**

1. With every `allow.*` off but `allow.suser` (Decision 9 read
   literally), the jail's root cannot bind a port below 1024 (`nc -l 22`:
   Permission denied). `allow.reserved_ports=1`, a kernel default, fixes
   it. Without it no sshd, web server, or DNS server can run in a jail.
2. `allow.raw_sockets` does nothing in a VNET jail: `ping` works with it
   off, as root and as nobody. hearst's own `kern_jail.c` grants
   `PRIV_NETINET_RAW` to every VNET jail before the `allow.raw_sockets`
   check. Every Skep jail is VNET, so the opt-in and its check cannot work
   as written.
3. `FreeBSD-set-minimal` has no sshd, so Decision 12's SSH key has
   nothing to enable. `FreeBSD-ssh` has no scripts or triggers (no
   allowlist change needed to add it).

**Decisions made.** None about the spec. As in step 6, I picked the
probe's location myself (a new skeptest dataset, destroyed afterwards).

**Stopped** under the run's rule: "If the spec is wrong, stop and report
options; don't work around it." Options are at the end of
step7-probe.txt. I recommend A1 (`allow.reserved_ports` stays on, like
`allow.suser`), B1 (drop the `allow_raw_sockets` opt-in and record that
VNET jails have raw sockets on their own stack), and C1 (the SSH key only
for a template with sshd, `FreeBSD-ssh` added to the default packages).

**hearst state:** no jails; `skeptest/step7-probe` destroyed; the template
has no clones; nothing deployed (still `a34749d`); no other change.

## 2. Andy's answers recorded (2026-11-16)

**Done.** A1, B1, C1, with Andy's additions. ADR 0010:

- Amendment f (Decision 9): `allow.reserved_ports` stays on with
  `allow.suser`; every other `allow.*` off, `set_hostname` and
  `unprivileged_proc_debug` included; the `allow_raw_sockets` opt-in is
  dropped, with the kernel source as the reason. Andy's addition: VNET
  jails can craft and spoof packets on their own epair, so the step 8 VLAN
  isolation test must include source-IP and source-MAC spoofing from a
  jail (added to Decision 16's "Jail networking" row).
- Amendment g (Decision 12): the SSH key only for a template with sshd,
  `container_ssh_unavailable` (422) otherwise; `FreeBSD-ssh` in the
  default template packages. Andy's addition: templates hold no SSH host
  keys, each jail makes its own at first start, and two jails from one
  template are checked to differ (new Decision 16 row "Jail SSH").
- The summary table, Decision 9's opt-ins, the "Jail baseline" check row,
  and a new "Step 7" section under the implementation notes.

**Approved for the build run:** deploy as often as needed (undo:
`a34749d`); a new template with `FreeBSD-ssh`, keeping `minimal-151`;
jails and datasets under skeptest; fresh API test accounts, passwords in
the scratchpad. Not approved, so not done: anything on hearst's network
configuration, `if_epair` (step 8), the VLAN test.
