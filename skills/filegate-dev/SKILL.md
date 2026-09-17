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

All operations address a named root and relative path. Index-free roots must not
need an xattr or hidden metadata index to work. Index rebuild replaces only
search rows; identity claims, current revision metadata, versions and sessions
are durable. Never delete the whole state directory to recover an index.

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

Update TS types/client, Go client, Fibel and skill references with every public
contract change. Raw SDK methods preserve non-success HTTP responses.

## Verification

Use `make test` locally, `make test-linux` for the actual Linux filesystem and
HTTP integration seam, `make test-race` on Linux, and `make docs`. Test durability
with injected failures and concurrency with channels, not arbitrary sleeps.
Keep `skills/filegate` and `docs-site/agent-skills/filegate` identical.
Use independent reviews for substantial persistence or security changes.
