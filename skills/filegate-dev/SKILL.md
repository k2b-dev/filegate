---
name: filegate-dev
description: Develop, test or review the Filegate repository, including its Linux daemon, root-scoped HTTP API, TypeScript and Go clients, Fibel docs and portable integration skill. Use this for changes to Filegate itself; use filegate for application integration or operations.
---

# Filegate development

Read current source and tests before changing behavior. The architecture is a
hard cut: no S3, detectors, admin app, dynamic configuration or compatibility API.
Do not reintroduce these through abstractions or fallbacks.

- `domain/`: root operations and Files/State ports; no imports of adapters/infra.
- `infra/filesystem/`: descriptor-relative Linux access, xattrs and reflink/copy.
- `infra/pebble/`: JSON state families and synchronous batches.
- `adapter/http/`: HTTP validation, bearer auth and signed transfer capabilities.
- `cli/`: static YAML, lifecycle and API-based administration.
- `api/v1/`: wire types and envelopes; `sdk/filegate/` and `sdk/ts/`: clients.
- `integration/`: Linux tests across real filesystem, state, HTTP and clients.
- `docs-site/`: Fibel docs and published portable skill.

## Invariants

Operations address a named root and relative path, or a stable ID for supported
reads. Public IDs, resolution and ID reads require `index && managed` per root;
report this as computed `RootInfo.stableIds`. Hide `Node.id` and `Version.fileId`
at HTTP response boundaries on other roots, including nested receipts and
transfer results; retain internal identities and histories unchanged. Index-free
roots must not need an xattr or hidden metadata index to work. Index rebuild
replaces only search rows; identity claims, current revision metadata, versions and sessions
are durable. Never delete the whole state directory to recover an index.

Stable IDs apply to files and directories. Newly assigned IDs use UUIDv7 in
`user.filegate.id`; existing IDs survive rebuild/restart. Same-root moves retain
descendant IDs, and overwrite/restore retain destination identity. New copies or
cross-root targets get new identities; explicit overwrite retains the existing
destination identity. Native move-overwrite retains source identity and removes
the replaced target's identity/history. Delete/recreate never reuses a deleted ID.

Recover root transitions before resolving an identity claim. ID content reads
and download issuance must resolve, open and verify under one root lock. Verify
identity on the opened descriptor. ID-issued leases sign current path and ID and
recheck the descriptor on use; never follow moves or serve a replacement identity.
Keep path-issued lease semantics and execution authorization unchanged.

All writes share atomic publication. A required old-content snapshot must succeed
before replacement. Metadata follows its content revision. Manual snapshots and
restore bypass cooldown. Root locks must not be held while reading network upload
bodies. Filesystem/state transitions require recovery records. API renames preserve
identity; copied xattrs must not steal histories.

New files inherit destination default ACLs and setgid groups; staged inodes do not
inherit on rename. Replace staging ACLs before publication. Overwrites and restore
retain ordinary permissions and access ACLs, but clear regular-file setuid/setgid.
All directory creation uses the same policy, including implicit parents and copies.
Explicit Mkdir prepares one directory privately and requires an existing parent;
its ownership and ACLs must be complete before publication.
Ownership overrides apply equally to direct uploads and resumable commits. ACL
scopes are independent and never recursive; unsupported ACLs must not disable
ordinary operations on index-free roots. Check actual permissions after setting
special bits, since Linux can silently clear setgid.

Session commit and lease renewal require backend authentication. Browser leases
permit only status/write and optionally abort, defaulting to 60 seconds and capped
at 300. Keep terminal session receipts for seven days; commit returns the original
Node independently of later path changes. Persist terminal state before cleanup.
Upload helpers never commit automatically. Index rebuild must retain receipts.

ZIP selections bind the exact manifest hash into a short-lived capability. Never
expand directory authorization beyond its selected subtree, expose private paths,
follow symlinks or silently omit failed file reads. Bound traversal, archive names,
manifest size, entry count, bytes and concurrent streams; stream without buffering
whole files. Lease expiry gates request admission, not accepted download duration.

Historical and thumbnail downloads reuse signed read leases and shared backend
renderers. Bind root, path and concrete version or normalized thumbnail dimensions;
never read scope overrides from browser query parameters. Version lookup must
verify the file identity; thumbnail paths retain current-file semantics.

Update TS types/client, Go client, Fibel and skill references with every public
contract change. Raw SDK methods preserve non-success HTTP responses.

## Verification

Use `make test` locally, `make test-linux` for the actual Linux filesystem and
HTTP integration seam, `make test-race` on Linux, and `make docs`. Test durability
with injected failures and concurrency with channels, not arbitrary sleeps.
Keep `skills/filegate` and `docs-site/agent-skills/filegate` identical.
Use independent reviews for substantial persistence or security changes.
