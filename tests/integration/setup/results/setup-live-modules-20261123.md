# Setup: live modules and the firmware dependency — langrishe, 2026-11-23

Branch `fix/setup-live-modules` (stacked on `chore/m10-backlog`), package built
from b959cc8 (`make stage` on merrick, `pkg create` on langrishe; staged
checksums equal). Unattended run; Andy away. hearst off, not contacted. Logs in
[setup-live-modules-20261123/](setup-live-modules-20261123/).

Host: langrishe, FreeBSD 15.1-RELEASE, pkg 2.9.2, Skep from 8fb036f before the
test, no guests (`jls`, `/dev/vmm` and `pgrep bhyve` empty), vmm, nmdm and the
kld_list modules loaded live.

## b) The package depends on edk2-bhyve

| Step | Result |
|---|---|
| `pkg delete -y edk2-bhyve` | removed; `/usr/local/share/edk2-bhyve` gone |
| `pkg add -f skep-0.1.0.pkg` | **refused**: `pkg: Missing dependency 'edk2-bhyve'`, exit 1; skep still the 8fb036f build |
| `pkg install -f -y ./skep-0.1.0.pkg` | `edk2-bhyve: g202508_3 [FreeBSD-ports] (required by skep)` installed first, then `skep: 0.1.0 [unknown-repository]`; exit 0; firmware files back; skep-setup at b959cc8 |
| `skep-setup upgrade -yes` (under `timeout -f`) | both services restarted at b959cc8, health ok, exit 0 |

So a local package file resolves the dependency through `pkg install ./file`,
which fetches it from the configured repositories; `pkg add` does not fetch it.

## a) Modules configured but not loaded

Answers `-mgmt vtnet0 -pool zroot` (langrishe's agent.json).

| Step | Result |
|---|---|
| `check` with every module loaded | no module item printed (it is `ok`); 17 ok, 0 warnings; exit 1 (an unrelated `add` and `manual` item) |
| `kldunload nmdm` (no guests) | exit 0; `kldstat -q -m nmdm` exit 1 |
| `check` | **warn kernel modules: configured but not loaded: nmdm**, "nmdm: VM serial consoles", `$ shutdown -r now`, `$ kldload -n nmdm`; 1 warning; exit 1, unchanged |
| `plan` | the same warning; exit 0 |
| `kldload -n nmdm` | exit 0; `kldstat -q -m nmdm` exit 0 |
| `check` | warning gone; 17 ok, 0 warnings; exit 1, unchanged |

Not run live: the end of `apply` and `upgrade`'s degraded message with
modules missing (unit tests only: `TestApplyReportsModulesNotLoaded`,
`TestUpgradeUnhealthyNamesModulesNotLoaded`). The `kern.racct.enable` add item
on langrishe predates this branch.

End state: nmdm loaded again; edk2-bhyve installed (g202508_3); Skep at b959cc8,
both services running; no guests.
