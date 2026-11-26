# Step 8c-2 review fixes, lab: the three-node cluster (2026-11-26)

smith (.81), langrishe (.82), star (.83), from package 0.1.0+1e19f57 to
the fix build **73a8218**. Drivers [8c2-labB.sh](8c2-labB.sh) and
[8c2fix-lab.sh](8c2fix-lab.sh) (each case one command, by hand); API
through [8c2-api.py](8c2-api.py) as the cluster's `admin`. Roles were read
before every case. Raw logs: `~/skep-test-logs/20261126T02*-8c2fix-L*` on
merrick.

| Case | Result |
|---|---|
| L1 rolling `skep-setup upgrade -yes -keep 50`, smith, langrishe, then the leader star | each exit 0, skep_cluster a new PID on 73a8218, three voters and health ok after each; import states unchanged (smith done, the others superseded); leadership moved to langrishe |
| L2 on the leader langrishe's own API remove langrishe | 200, `handed_over: true`; smith and star two voters, health ok; langrishe REMOVED. `leave --wipe -cluster-id`: exit 0, new cluster ID and key; `join-request`; admit on smith's API: 200, the pin equal to the new key's; join: three voters, health ok after 3 s. (The driver's first join lacked `-cluster-id` and was refused, "no terminal to confirm on", nothing changed; fixed, the join ran once) |
| L3 smith `skep-setup remove -plan` | refused, exit 3: "this node is not shown to be alone in cluster ff712cb0-…: the Raft configuration has 3 servers. If it is one of several members, remove it on another member, then wipe it here", with `skep-cluster remove smith.greenbean.org` and `skep-cluster leave --wipe`; PIDs unchanged |
| L5 end | three voters in state member, health ok, nothing pending, `audit verify` 0 on all three, vid tunable 0 on all three, nothing root-owned in `/var/db/skep/cluster`; leader star |

End state: smith, langrishe, star on 0.1.0+73a8218; leader star.

## Merge-run check (2026-11-26): init -check opens no file as root, build 5cb6e3b

Rolling `skep-setup upgrade -yes -keep 50` from 73a8218 to **5cb6e3b**
(leader star read first and upgraded last). Raw log:
`~/skep-test-logs/20261126T095844Z-8c2merge-lab-upgrade.log` on merrick.

- smith: upgrade exit 0; three voters member, health ok; `audit verify` 0; vid 0; import state done; `init -check` "initialised".
- langrishe: upgrade exit 0; three voters member, health ok; `audit verify` 0; vid 0; import state superseded; `init -check` "initialised".
- star (leader before, upgraded last; leadership moved to langrishe): upgrade exit 0; three voters member, health ok; `audit verify` 0; vid 0; import state superseded; `init -check` "initialised".
- blazanov (single host): upgrade exit 0; one-node cluster leader, health ok; vid 0; import state done; `init -check` "initialised".
