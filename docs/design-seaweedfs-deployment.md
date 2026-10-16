# Design: deploying SeaweedFS on the homelab

Status: design note, 2026-10-16, written against ADR 0118 (accepted). **No cluster changes are
made here and none should be attempted** - the user will review this note before anything is
installed. No product code changed. Checked against the two SeaweedFS spikes
(`docs/design-homelab-warehouse-store.md`, `docs/design-seaweedfs-iam-spike.md` commit `b3c7863`,
both branch-only, unmerged), `booth-architecture/ARCHITECTURE.md` items 66-67 (the existing Garage
deployment for ADR 0109 off-cluster backups, and the MinIO-unmaintained finding that started this
whole line of work), and `hack/docker-compose.emulators.yml`'s own image-pinning convention.

## Scope: this is a different SeaweedFS from any existing homelab component

There is no SeaweedFS on the homelab today. Item 67 of `ARCHITECTURE.md` records a *different*
store - **Garage v2.4.1, namespace `booth-backup`, on server 2** - already deployed for ADR 0109's
off-cluster Postgres backup destination (`deployment/homelab/garage-backup-store.yaml`). That
deployment is unrelated to this one: it backs up Postgres dumps, not Spark/Lakehouse data, and
Garage was explicitly ruled out (both spikes; ARCHITECTURE item 67(c)) for anything needing
create-only writes or scoped credentials - which is exactly what Spark/Lakehouse need. **This note
is about a new, second object store, serving a different purpose, almost certainly on the main
cluster rather than server 2** (where Spark and Lakekeeper actually run) - but the physical
placement is the user's call, not inferred here.

## Namespace

Following the fleet's own convention - each component gets a namespace dedicated to it, named for
what it serves rather than the software itself (`booth-backup` for the backup-destination Garage,
not `garage`) - this note suggests a namespace named for the role: something like
`booth-lakehouse-storage` or `seaweedfs-lakehouse`, distinct from both `booth-storage`'s own
namespace (this module's chart - `charts/booth-storage/values.yaml` already documents "install
this chart into a namespace dedicated to booth-storage") and `booth-backup` (the unrelated Garage
instance). The exact name is a naming decision for the user to make, not inferred here; what
matters structurally is that it is its own namespace, not shared with `booth-storage`,
`booth-lakehouse`, or `booth-spark` - those modules only ever reach it as an S3 endpoint through
credentials this module's broker mints, never through direct namespace-level access.

## Persistence

A real (non-single-process-spike) SeaweedFS deployment separates three roles, each with different
persistence needs:

- **Master** (topology, volume assignment, Raft log): small state, but losing it without a backup
  means the cluster can't reconstruct which volume server holds which data on restart. Needs a
  small persistent volume per master replica.
- **Volume servers** (the actual object bytes): the bulk of the storage footprint. Needs a
  persistent volume sized for whatever Spark/Lakehouse data this is meant to hold, with whatever
  storage class the homelab uses for bulk data (same class `booth-backup`'s Garage or this
  module's own filesystem-kind backends would use, if any - not independently checked this pass).
- **Filer** (metadata: the directory tree, and - confirmed directly in both spikes' startup logs,
  `"Registered IAM gRPC service on filer"` - **IAM identities, policies, and access keys live in
  the filer's own metadata store, not the master**). This is the one that matters most for this
  ADR's purpose: **losing the filer's metadata store loses every IAM user, policy, and access key
  this module has ever minted**, not just unflushed writes. SeaweedFS's filer supports multiple
  backing stores for this metadata (its default is an embedded LevelDB-style store on local disk;
  it also supports an external database, including PostgreSQL, via its `filer.toml`
  `[postgres]`/`[postgres2]` sections - general SeaweedFS knowledge, not independently verified
  against this spike's single-process container, which used the embedded default).

**Recommendation worth the homelab operator's consideration**: point the filer's metadata store at
a dedicated PostgreSQL database rather than the embedded default. This module already runs
Postgres for its own `registry`/`uploadgrant`/lease tables (section 4 of
`docs/design-seaweedfs-provider.md`), and the existing ADR 0109 backup chain (item 66:
`booth-core-postgresql-backup`, nightly, already proven with a working restore drill) already
knows how to back up Postgres databases - reusing that path for the filer's metadata means IAM
state recovers through the exact same, already-tested mechanism as every other database on this
homelab, instead of needing a separate backup strategy for an embedded on-disk store. This is a
recommendation, not a verified-working configuration - it wasn't exercised in either spike, both of
which used the embedded default for speed.

## Volume sizing

The gate-4 spike (`docs/design-seaweedfs-gate4.md`, branch `design-seaweedfs-gate4`, commit
`6db2104`) directly confirmed a concern the coordinator flagged for the planned 50GB initial
volume: SeaweedFS's default `-master.volumeSizeLimitMB` is `30720` (30GB), and on a disk this
small relative to that default, the volume-growth algorithm refuses to allocate new volumes once it
judges there isn't enough free-space headroom left - observed directly as `No writable volumes and
no free volumes left`, reproducibly, well before the disk is actually full. This is **not** a
`-volume.max` slot-count problem (that flag defaults to a fixed `8`, not auto-scaled by disk size) -
it's the per-volume size assumption itself that needs to shrink to match a small disk.

**Recommended setting: `-master.volumeSizeLimitMB=1024`** (1GB) for this deployment's 50GB volume.
The gate-4 spike confirmed, at a smaller scale (a 20MB test disk), that lowering this flag measurably
increases how much of the real disk is usable as volume slots before the same failure mode is hit
(two real volumes successfully allocated at a 10:1 disk-to-limit ratio, versus zero at the default
30720MB limit on the same disk) - the exact 50GB-scale numbers weren't re-measured, but the
mechanism and the direction of the fix are now directly confirmed, not just inferred from
documentation. Set this flag on the master (or on `weed server` if master and volume roles are
combined); re-check it if the volume's provisioned size changes.

## Version pin

Both spikes ran `chrislusf/seaweedfs:latest`, which resolved to **build `30GB 4.48` (`530be3e37`)**
at the time of testing - the exact string `weed`'s own startup log prints
(`Start Seaweed Volume server 30GB 4.48 530be3e37`). `:latest` is exactly the kind of tag this
repo's own test tooling has already learned not to trust: `hack/docker-compose.emulators.yml`
pins its MinIO test image **by digest**, not tag, with an explicit comment explaining why
(`ADR 0087/0091`, and item 67's own finding that MinIO's tags and even specific digests can
disappear from a registry entirely). The same discipline applies here, more urgently, since
this note is proposing a production homelab component, not a test fixture: **pin
`chrislusf/seaweedfs@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d`** -
the exact digest both spikes actually ran, confirmed directly: `chrislusf/seaweedfs:4.48` and
`:latest` resolved to this identical image ID at the time this note was written, so the registry
does publish an immutable-looking per-version tag today, but pin the digest anyway, not the tag -
a tag can be moved to point at a different image later even if it hasn't been yet (the same thing
`ADR 0087/0091` already learned the hard way about MinIO's tags), the same way
`MINIO_TEST_IMAGE`'s default value is pinned, so a later `docker pull` of "the same tag" can't
silently change what's actually running underneath an already-validated deployment.

## IAM state surviving a restart

Directly follows from the persistence section above: IAM state lives in the filer's metadata
store, so a restart that preserves the filer's persistent volume (or external Postgres database,
if the recommendation above is taken) preserves every IAM user, policy, and access key this
module has minted - unexpired leases keep working, and the sweeper (`docs/design-seaweedfs-
provider.md` section 4) continues to find and clean up whatever it already knew about. A restart
that loses the filer's metadata store (ephemeral storage, a misconfigured volume, or a filer
pointed at a fresh, empty backing store) **silently invalidates every outstanding lease at once**:
SeaweedFS no longer recognizes any of the access keys, so every request using one starts failing
with `AccessDenied` or similar - this module's own lease table still thinks they're valid until
their `expires_at` passes, at which point the sweeper's `DeleteAccessKey`/`DeleteUser` calls would
get a clean "no such user" (already handled as success, section 4 step 3), so the table
self-corrects *eventually*, but there's a window where this module believes leases are live that
SeaweedFS has already forgotten. This is a safe failure mode (no credential outlives real server
state - if anything, it fails *before* its advertised expiry, never after), but it does mean an
unplanned filer-data loss is effectively "every Spark/Lakekeeper job currently running loses its
storage access immediately," worth surfacing to whoever operates this if it ever happens.

## Backup

Two genuinely different things need backing up, with different stakes:

- **The filer's metadata store** (IAM state, directory tree) - per the persistence section above,
  if this is moved to Postgres, it rides along with the existing, already-proven ADR 0109 backup
  chain for free. If left on the embedded default, it needs its own backup strategy - SeaweedFS
  has no backup tooling of its own for this beyond "back up the volume the embedded store lives
  on," which is a weaker guarantee than Postgres's own dump-based mechanism (no "restore drill"
  equivalent has ever been exercised against SeaweedFS's embedded filer store, unlike Postgres's,
  which item 66 already proved works).
- **The actual object data** (volume servers) - this is Spark/Lakehouse's real data, not metadata,
  and is likely far larger than anything else this homelab currently backs up. SeaweedFS's own
  replication setting (`-volume.replication`, not exercised in either spike - both ran a single
  volume server with no replication) provides redundancy *within* a running cluster, not a backup:
  losing the only copy of a volume loses the data regardless of replication factor if there was
  only ever one copy. Erasure coding (`volume.ec.encode`) is a separate, optional SeaweedFS feature
  for cold-storage space efficiency, not a backup mechanism either - worth distinguishing clearly
  for whoever eventually writes this homelab's actual backup job, since both terms sound like they
  might substitute for an off-cluster backup copy and neither does.

Neither of these was exercised in either spike (single-node, ephemeral containers, by design); this
section is a map of what needs a real backup strategy before this store holds anything that
matters, not a tested one.

## Upgrade path

Both spikes happened to run the same build (`30GB 4.48`), so there is no cross-version comparison
data yet - a future version bump has nothing to check against except re-running the same probes
from scratch. **Recommended validation before cutting over a production instance to a new
SeaweedFS version**: re-run the exact conformance checks both spikes already wrote down against
the new version before pointing anything real at it - the create-only probe (`docs/design-homelab-
warehouse-store.md` section 2), the IAM user/policy/key flow and prefix-scoping check
(`docs/design-seaweedfs-iam-spike.md` section 1), and the full S3A/Lakekeeper operation battery
(section 2 of the same note) - the same discipline this repo's own `backendtest` conformance suite
already applies to every storage kind it supports, just manually, since SeaweedFS isn't a
registered kind in that suite (it's the same `s3.go` code path other S3-compatible stores use;
nothing here suggests adding a SeaweedFS-specific automated conformance run, since the risk is in
*behavior changing between SeaweedFS versions*, not in this module's own code).

**Known risk areas to watch across a version bump**, based only on what both spikes actually
observed changing behavior on (not a general SeaweedFS changelog review):

- **The IAM/STS authorization model** - the second spike found a real, version-sensitive-feeling
  behavior (a base identity needs a real attached IAM policy document, not just the legacy
  `actions` list, for `GetFederationToken` to work) that the first spike didn't know to look for.
  If this module ever does build the `GetFederationToken` path (not designed in
  `docs/design-seaweedfs-provider.md`, since nothing needs it yet), this is exactly the kind of
  thing a version bump could silently change again.
- **The S3 API's checksum/precondition handling** - this note's own section 1 found genuine
  `aws-chunked` trailer support and consistent `If-None-Match`/`If-Match` handling at `4.48`; an
  upgrade is the natural place to re-confirm both haven't regressed, given how specific and
  easy-to-silently-break this class of behavior has already proven to be (the MinIO header-matching
  bug found earlier in this module's own history, ADR 0117's build task, is the same category of
  risk).

## What I could not verify

- ~~SeaweedFS's filer-backed-by-Postgres configuration~~ - now exercised directly: the gate-4
  spike (`docs/design-seaweedfs-gate4.md`) got `[postgres2]` working (confirmed via `weed scaffold
  -config=filer` on the pinned image), and both a restart and a `4.47`->`4.48` upgrade preserved
  IAM state against it. Still open: the Postgres-filer collation warning gate 4 observed
  (`filemeta.name collation "en_US.utf8" is not byte-ordered`, falls back to a slower `COLLATE
  "C"` sort) - a potential effect on S3 listing order for Lakekeeper/Spark, not investigated.
- Any real multi-node SeaweedFS deployment (separate master/volume/filer processes, replication
  across volume servers) - both spikes ran a single `weed server` process with every role
  combined, which is adequate for proving the S3/IAM mechanisms work but says nothing about
  production-scale topology, replication behavior, or failover.
- Actual backup/restore of SeaweedFS's own data (filer metadata or volume bytes) - never
  attempted; this note only maps what would need it.
