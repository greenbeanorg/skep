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
