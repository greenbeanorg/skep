# main-c60571a

Written by the build of main c60571a (step 8d-1) with
`golden_gen_test.go.txt`, copied into that tree's
`internal/cluster/clusterstate/` and run with
`GOLDEN_OUT=<dir> go test -run TestWriteGoldenLog ./internal/cluster/clusterstate/`:

- `log.jsonl`: 18 version 1 entries (`{"index":N,"entry":<encoded entry>}`),
  a join, guests, a refused and an applied removal, no member.join with
  guests;
- `state.json`: main's snapshot after applying them;
- `audit-00000001.jsonl`: the cluster audit main wrote while applying them.

Step 8d-2's test `TestMainLogAppliesAsBefore` applies the log with this
build and compares (D27).
