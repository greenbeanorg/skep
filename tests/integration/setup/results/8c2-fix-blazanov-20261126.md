# Step 8c-2 review fixes, lab: blazanov (2026-11-26)

blazanov = VM 944 on garrett (10.79.20.84), from 0.1.0+1e19f57 to the fix
build **73a8218**. Driver [8c2-labA.sh](8c2-labA.sh). Raw log:
`~/skep-test-logs/20261126T0220*-8c2fix-L4.log` on merrick.

| Step | Result |
|---|---|
| upgrade to 73a8218 (`pkg install`, `skep-setup upgrade -yes -keep 50`) | exit 0; one-node cluster, leader, health ok, import done, nothing root-owned |
| `skep-setup remove -yes` | the cluster item "alone: cluster …, 1 member(s), leader known, health ok"; exit 0; three services stopped, the three `_enable` lines `NO` |
| `apply -mgmt vtnet0 -pool zroot -no-racct -start -yes` (F4) | refused, exit 3, "nothing was changed": `start skep_agent`, `start skep_cluster`, `start skep_api` each "skep_X_enable is "NO", perhaps from an earlier remove: set it to YES (sysrc skep_X_enable=YES), then run apply again"; rc.conf and loader.conf SHA-256 unchanged, no new journal, nothing running |
| `qm rollback 944 skep-01`, `qm start 944`, install 73a8218, `skep-setup upgrade -yes -keep 50` | exit 0: `skep_cluster_enable` added, import pending, `skep-api admin import-users`, import done; one-node cluster, health ok, `audit verify` 0, nothing root-owned |

End state: blazanov up, 0.1.0+73a8218, one-node cluster, import done;
snapshots `fresh-blazanov` and `skep-01` kept.
