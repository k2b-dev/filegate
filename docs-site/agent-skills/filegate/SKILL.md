---
name: filegate
description: >
  Integrate, configure, operate, or develop Filegate, the Linux file gateway
  with REST, TypeScript, Go, and S3-compatible APIs. Use this skill for file
  uploads and downloads, stable node IDs, virtual paths, resumable sessions,
  direct browser transfers, S3 clients and keys, declarative configuration,
  versioning, storage operation, deployment, recovery, or Filegate source
  changes.
---

# Work with Filegate

Filegate exposes regular Linux files through indexed REST, TypeScript, Go, and
S3-compatible surfaces. Applications keep their users, permissions, and
product model. Filegate owns filesystem-backed file operations, stable IDs,
metadata lookup, transfers, versions, and operational state.

Use the following gates for Filegate work. They keep exact details in the
current documentation instead of duplicating a second API reference here.

## 1. Read the current contract

Prefer the Filegate documentation MCP when it is available. Search with
`search_docs`, then read the smallest matching page with `read_doc` before
making exact claims about routes, types, configuration, defaults, errors, or
deployment behavior.

For a local documentation server on the default port, the endpoint is:

```text
http://localhost:5173/docs/_fibel/mcp
```

If the MCP connection is unavailable, read the matching Markdown below
`docs-site/docs/en` and the public Go or TypeScript types directly. State when
the task is using this reduced documentation mode.

**Gate:** exact behavior comes from current documentation or public source.

## 2. Choose the access surface

- Use the REST API or an SDK for application file UX, metadata, stable IDs,
  direct URLs, upload sessions, search, versions, and administration.
- Use the TypeScript SDK from Bun or Node application servers and trusted
  internal tools.
- Use the Go SDK from Go services and operators.
- Use path-style S3 for existing backup, sync, migration, and object-storage
  clients.
- Use the Admin app for full-authority operator workflows.

Do not expose the REST bearer token to a public browser. The application server
authorizes the user and creates scoped direct upload or download credentials;
the browser transfers bytes with those scoped credentials.

**Gate:** the selected surface matches the caller and its trust boundary.

## 3. Preserve identity and conflict behavior

Persist Filegate node IDs rather than paths. IDs remain stable across moves and
renames; paths do not.

Choose conflict behavior explicitly. One-shot and session writes do not
silently overwrite by default. Read the current upload or API documentation for
the supported `onConflict` values and response diagnostics.

Use upload sessions for large or resumable transfers. Small files can use the
one-shot path, and direct browser flows should keep the application backend out
of the byte stream.

**Gate:** callers preserve stable identity and handle conflicts deliberately.

## 4. Respect the security boundary

The REST bearer token grants full file and configuration authority for the
Filegate instance. Keep it in trusted services and secret stores.

- Run REST and Admin surfaces on a trusted network behind TLS termination.
- Use scoped S3 keys when an S3 client needs access to selected buckets.
- Use direct-transfer tokens for browser uploads and downloads.
- Treat `X-Filegate-Actor` as activity metadata, not authorization.
- Apply REST request limits at the reverse proxy.

Never infer application authorization from Filegate paths, mount names, or
activity labels. The application owns user and product permissions.

**Gate:** no master credential crosses into an untrusted client.

## 5. Manage configuration and runtime resources

Keep service behavior in a versioned manifest and use `fg config plan` before
`fg config apply`. The manifest is complete desired state, not a patch.

Bootstrap-owned values are available before the runtime store opens. Secrets
and runtime resources such as S3 keys stay outside the manifest. A static
manifest change becomes effective after restart; a runtime change is published
after apply.

Use the dedicated S3 key API or Admin S3 page to create, rotate, scope, disable,
and delete runtime access keys.

**Gate:** declarative settings, bootstrap inputs, and runtime resources keep
their separate ownership and lifecycle.

## 6. Operate the storage safely

Run one Filegate daemon per writable mount set, runtime store, and Pebble index.
Do not share those embedded stores between active processes.

Back up:

1. every configured data root, including xattrs and internal Filegate
   directories;
2. the runtime config store containing applied desired state and generated
   credentials;
3. bootstrap files and external secrets;
4. the metadata index only when captured consistently with the file tree.

The filesystem holds file bytes and stable-ID xattrs. The runtime store is not
rebuildable from those files. The metadata index can be rebuilt offline, but an
index rebuild loses in-progress upload-session metadata.

Use authenticated readiness and system-information routes after deployment,
storage changes, restore, and upgrade. Read the operations documentation for
the current probes and recovery commands.

**Gate:** one daemon owns the storage and every authoritative state class is
covered by backup and restore.

## 7. Verify changes through a public seam

For integration code:

1. construct the client for the trusted runtime;
2. perform the operation through a public API;
3. handle status, stable error information, and conflict diagnostics;
4. read the result back or run a focused health check.

For Filegate source changes, update the Go contract and both SDKs when the
public HTTP surface changes. Update the canonical Fibel page for observable
changes to APIs, defaults, errors, permissions, configuration, deployment, or
operations. Review the focused diff and run the smallest test that proves the
affected public seam.

**Gate:** implementation, SDKs, documentation, and verification agree.
