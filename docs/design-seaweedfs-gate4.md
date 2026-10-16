# ADR 0118 gate 4: SeaweedFS restart/upgrade IAM persistence, and a volume-sizing check

Status: spike results, 2026-10-16. No product code changed (`cmd/`, `internal/` untouched). No
cluster changes - everything here ran in local Docker, torn down before this note was written.
Gate 4 is amended (per the coordinator's 2026-10-16 ADR 0118 amendment) to gate the homelab
*install* of SeaweedFS, not the `seaweedfs` provider code, which a separate, parallel session is
building against the already-reviewed `docs/design-seaweedfs-provider.md` design.

**Setup used throughout**, confirmed from the two prior spikes (`docs/design-homelab-warehouse-
store.md`, `docs/design-seaweedfs-iam-spike.md` commit `b3c7863`): `weed server -s3
-s3.ip.bind=0.0.0.0 -s3.iam -s3.iam.readOnly=false -s3.iam.config=/etc/seaweedfs/iam.json
-dir=/data -ip.bind=0.0.0.0`, with `iam.json` = `{"sts": {"signingKey": "<32 random bytes,
base64>"}}` (required even for plain non-STS IAM calls). **One change from the prior spikes'
method**: seeding the first admin identity. Both prior spikes used `echo 's3.configure ...' | weed
shell`; in this environment (Windows + Docker Desktop, via Git Bash) `weed shell` never produced
any output or exited under any invocation tried (piped stdin, `docker attach` to a detached
container, `--help`, 15-90s waits) - consistently zero bytes on stdout/stderr, looking identical to
a hang. Rather than spend more of this time-box diagnosing a tooling quirk, this spike used
SeaweedFS's other, equally-real bootstrap mechanism instead: the static `-s3.config=<path>` flag (a
JSON file of initial identities, confirmed via `weed server -h`), which seeds an admin identity at
startup with no `weed shell` step at all:
```json
{"identities": [{"name": "admin", "credentials": [{"accessKey": "admin_key", "secretKey":
"admin_secret_1234"}], "actions": ["Admin"]}]}
```
This is a legitimate, independently-documented SeaweedFS mechanism (not a workaround invented for
this spike), and arguably more production-realistic than an imperative `weed shell` command anyway
(a static, GitOps-friendly config instead of a one-time imperative step) - but it means this spike
did not re-attempt `weed shell` itself, so I can't say whether that tool's apparent hang is real or
an artifact of this specific host. All IAM mint/verify probes below used a small Go harness built
against the real AWS SDK for Go v2 (`service/iam`, `service/s3`), not botocore - direct SigV4
calls, same approach the already-reviewed `docs/design-seaweedfs-provider.md` section 1 used for
its trailer-checksum test.

## 1. Restart persistence

Both backends: boot fresh, seed admin, create an IAM user + prefix policy (`incoming/*` in bucket
`gate4`) + access key, confirm the key is allowed inside the prefix and refused outside it and at
the bucket root, restart the same container with the same storage attached, then re-run the exact
same allow/deny probes against the same key.

| Backend | Result |
|---|---|
| Embedded filer (leveldb2) on a persistent bind-mounted directory | **PASS.** `docker restart` on the same volume: raft re-elects itself leader and the S3 API server is back up in ~17 seconds (confirmed from timestamps: restart at `16:32:46`, "becomes leader" at `16:33:02`, S3 API server listening at `16:33:03`). The pre-restart IAM user's key still authenticates and is still correctly scoped (allow inside prefix, deny outside, deny at root) - all four probes passed identically before and after. |
| Filer backed by PostgreSQL (`[postgres2]` in `filer.toml`, confirmed via `weed scaffold -config=filer` on the pinned image itself, not assumed) | **PASS.** Same restart procedure, same result: the IAM user/key minted before the restart is intact and correctly scoped after it. `configured filer store to postgres2` confirmed in the startup log (not silently falling back to the embedded default). |

Both backends: IAM state (users, policies, access keys) survives an ordinary container restart
when the backing store (bind-mounted directory, or the external Postgres database) survives too -
consistent with the already-reviewed deployment note's claim that this state lives in the filer's
metadata store, now independently confirmed for both concrete backends ADR 0118 asked about, not
just inferred from startup log lines.

**One real, reproducible failure mode worth flagging, found along the way**: recreating the
embedded-filer container with a *stale/partially-written* data directory (left over from an earlier
container that was force-removed mid-startup, in this spike's own trial and error) leaves the
single-node raft cluster stuck permanently retrying `rpc error: ... raft.Server: Not current
leader` - it never self-heals, unlike a clean restart. This is not a gate-4 pass/fail finding (it
was self-inflicted by iterating on this spike's own setup, not a property of a normal restart), but
it is a real operational risk worth naming in `docs/design-seaweedfs-deployment.md`'s own backup/
restore section: an unclean shutdown that corrupts the embedded store's on-disk state may need the
volume wiped and IAM state re-minted from scratch, not just a restart.

## 2. Upgrade persistence

No SeaweedFS release newer than `4.48` exists as of 2026-10-16: confirmed directly against the
SeaweedFS GitHub releases API (the ten most recent releases top out at `4.48`, published
2026-09-28) and against Docker Hub (`chrislusf/seaweedfs:latest` resolves to the identical digest
as the pinned `chrislusf/seaweedfs:4.48`, i.e. `sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99
ade2fc8c8bfce4d7872d` - confirmed by pulling both and comparing). There is therefore nothing newer
to upgrade *to* right now. The most recent real version transition available to test is `4.47`
(published 2026-09-14, digest `sha256:ce9e796f1fe6f06968f4c04bdaf8f678dad9c8acdfef3d244133d71bfa6
bf882`) **upgrading to** `4.48` - a genuine prior-version-to-pinned-version transition, not a
same-version no-op. This is what was tested; it needs re-running against whatever is actually
newer whenever the homelab's SeaweedFS version is next bumped past `4.48`.

| Backend | Result |
|---|---|
| Embedded filer (leveldb2) | **PASS.** Fresh `4.47` instance: seeded admin, minted an IAM user/key, confirmed scoping. Stopped the container, started `4.48` on the *same* bind-mounted directory. S3 API server came back up normally; the `4.47`-minted key still authenticates and is still correctly scoped on `4.48` - all four probes passed. |
| Filer on PostgreSQL | **PASS.** Same procedure: fresh `4.47` instance with the filer pointed at a fresh Postgres database, mint+verify, stop, start `4.48` against the *same* Postgres database and the *same* `-dir` volume. The `4.47`-minted key still works identically on `4.48`. |

No schema or behavior break observed across this one version step, for either backend. This says
nothing about a larger version jump (e.g. several releases at once, if the homelab skips versions)
- only this specific, real, adjacent-version transition was exercised.

## 3. Volume-size-limit finding (Task B)

The coordinator's flagged concern: a 50GB homelab disk against SeaweedFS's default `-master.
volumeSizeLimitMB` (confirmed via `weed server -h`: default `30720`, i.e. 30GB) might yield too few
usable "volume slots," causing `no writable volumes` failures as new buckets are written to.

**This is real, confirmed directly - but the actual mechanism is slightly different from "too few
slots," and matters for what fix to apply.** `-volume.max` (how many volumes a volume server will
host) defaults to a **fixed `8`**, not auto-scaled from disk size, unless explicitly set to `0`
(auto). So on a small disk, the limiting factor isn't hitting a slot *count* ceiling - it's that
SeaweedFS's volume-growth algorithm refuses to allocate a **new** volume once it judges there isn't
enough free disk headroom left, and that judgment is relative to `-master.volumeSizeLimitMB`
regardless of how many slots remain under the `8`-slot cap. Observed directly, scaled down to a
20MB `tmpfs`-backed data directory (a real, enforced size cap via Docker, not a simulated one) to
keep this spike fast - the ratio to the real 50GB target isn't preserved exactly, but the mechanism
being tested (free-space-relative volume growth refusal) is the same regardless of absolute scale:

| Config | Result, same 20MB disk |
|---|---|
| Default `-master.volumeSizeLimitMB=30720` (untouched) | First 5MB object: succeeds (the one volume SeaweedFS auto-grows at startup absorbs it). **Second** 5MB object, different bucket: fails - server log: `create 7 volume, created 0: Not enough data nodes found!` immediately followed by `No writable volumes and no free volumes left`. Only one bucket's worth of data fit before the growth algorithm refused to create any more volumes on this disk, full stop. |
| Tuned `-master.volumeSizeLimitMB=2` (a 1/15360 scale-down of the default, for this 20MB test disk) | First two 1MB objects (different buckets): **both succeed**. Third: fails, same error signature (`No writable volumes and no free volumes left`), but the log this time shows `create 7 volume, created 2` - the smaller per-volume cap let the growth algorithm actually allocate 2 real volumes before running out of headroom, versus 0 with the default. |

**Reading this honestly**: lowering `-master.volumeSizeLimitMB` does not remove the ceiling - a
finite disk is still a finite disk, and more buckets/objects than that disk can hold will always
eventually fail. What it demonstrably does is let **more of the real disk actually get used as
usable volume slots** before that ceiling is hit, because each volume's growth check needs much
less assumed headroom. For a 50GB homelab disk, scaling the same ratio tested here
(`20MB disk : 2MB limit` is a 10:1 ratio) up proportionally supports the design note's prior
*recommendation* of roughly `-master.volumeSizeLimitMB=1024` (1GB) on a 50GB disk (a ~49:1 ratio,
more conservative than the 10:1 this spike confirmed works) - **this spike confirms the mechanism
and the direction of the fix, not the exact number for 50GB specifically** (not re-tested at that
literal scale - see below).

**Recommended setting for the homelab**: `-master.volumeSizeLimitMB=1024`, as the already-reviewed
deployment note already proposed - this spike adds direct confirmation that lowering this flag is
the correct lever (not `-volume.max`, which doesn't auto-scale by default and wasn't the actual
constraint in either test above) and that the failure mode it's meant to prevent
(`no writable volumes and no free volumes left`) is real and directly reproducible, not merely
theoretical.

## 4. What I could not verify

- `weed shell`'s apparent hang on this host (Windows + Docker Desktop via Git Bash) - worked around
  with the static `-s3.config` identity file instead (section above), but the root cause of the
  hang itself (host-specific terminal/stdin handling vs. a real regression) was not diagnosed.
- The volume-sizing finding (section 3) at the literal 50GB scale - tested at a 20MB/2MB-vs-30720MB
  scale for speed; the mechanism (free-space-relative volume growth refusal) should hold at any
  scale, but the exact number of usable buckets/objects before hitting the ceiling at 50GB with
  `-master.volumeSizeLimitMB=1024` was not directly measured.
- A real multi-node SeaweedFS deployment, or any topology beyond a single `weed server` process -
  both this spike and both prior ones ran single-process setups.
- Whether the Postgres-backed filer's collation warning (`postgres: filemeta.name collation
  "en_US.utf8" is not byte-ordered, so S3 list order is not byte-lexicographic... Falling back to a
  slower COLLATE "C" sort`, seen directly in this spike's logs) has any practical effect on
  Lakekeeper/Spark listing behavior - flagged for `docs/design-seaweedfs-deployment.md` to note if
  the Postgres-filer recommendation is taken, not investigated further here (out of this spike's
  scope).
