import type { VersionResponse } from "@k2b/filegate";
import { formatBytes, formatUnix } from "../lib/format";
import { Icon, IconLabel } from "../components/Icons";

/**
 * Per-file version history.
 *
 * Filegate captures versions on overwrite and exposes eight endpoints for them;
 * none of it was reachable from the UI, so the feature was effectively curl-only.
 */
export function Versions(props: { fileId: string; parentPath: string; versions?: VersionResponse[]; unsupported?: boolean }) {
  if (props.unsupported) {
    return (
      <div class="panel">
        <div class="panel-head">
          <h2>Versions</h2>
        </div>
        <div class="panel-body">
          <p class="muted">
            Versioning is not active. In <code>auto</code> mode every mount must support reflinks; set <code>versioning.enabled</code> to <code>on</code> to allow full byte copies instead.
          </p>
        </div>
      </div>
    );
  }

  const versions = props.versions ?? [];
  // Newest first: the server returns oldest first, but the interesting version
  // when you open this panel is almost always the most recent one.
  const ordered = [...versions].sort((a, b) => b.timestamp - a.timestamp);

  return (
    <div class="panel" style="view-transition-name: fg-versions">
      <div class="panel-head">
        <h2>Versions</h2>
        <button
          class="btn"
          type="button"
          data-snapshot-open
          data-snapshot-id={props.fileId}
          data-snapshot-parent={props.parentPath}
        >
          <IconLabel icon="camera">Snapshot now</IconLabel>
        </button>
      </div>
      <div class="panel-body">
        {ordered.length === 0 ? (
          <p class="muted">No versions captured yet. Overwriting this file records one, or take a snapshot.</p>
        ) : (
          <ul class="versions">
            {ordered.map((version) => (
              <li class={`version${version.pinned ? " pinned" : ""}`}>
                <div class="version-row">
                  <div class="version-when">
                    <strong>{formatUnix(version.timestamp)}</strong>
                    <span class="muted">{formatBytes(version.size)}</span>
                  </div>
                  <div class="version-tags">
                    {version.pinned && <span class="tag pin"><Icon name="pin-filled" /> Pinned</span>}
                    {version.deletedAt ? <span class="tag warn">Source deleted</span> : null}
                  </div>
                </div>
                {version.label && <div class="version-label">{version.label}</div>}
                <div class="version-actions">
                  <a class="btn" href={`/files/versions/download?id=${props.fileId}&versionId=${version.versionId}`}>
                    <IconLabel icon="download">Download</IconLabel>
                  </a>
                  <button
                    class="btn"
                    type="button"
                    data-restore-open
                    data-restore-id={props.fileId}
                    data-restore-version={version.versionId}
                    data-restore-parent={props.parentPath}
                    data-restore-when={formatUnix(version.timestamp)}
                  >
                    <IconLabel icon="history">Restore</IconLabel>
                  </button>
                  <form method="post" action={version.pinned ? "/files/versions/unpin" : "/files/versions/pin"}>
                    <input type="hidden" name="id" value={props.fileId} />
                    <input type="hidden" name="versionId" value={version.versionId} />
                    <input type="hidden" name="parentPath" value={props.parentPath} />
                    <button class="btn" type="submit">
                      <IconLabel icon={version.pinned ? "pin-filled" : "pin"}>{version.pinned ? "Unpin" : "Pin"}</IconLabel>
                    </button>
                  </form>
                  <form
                    method="post"
                    action="/files/versions/delete"
                    data-confirm-version-delete={formatUnix(version.timestamp)}
                  >
                    <input type="hidden" name="id" value={props.fileId} />
                    <input type="hidden" name="versionId" value={version.versionId} />
                    <input type="hidden" name="parentPath" value={props.parentPath} />
                    <button class="btn danger" type="submit">
                      <IconLabel icon="trash">Delete</IconLabel>
                    </button>
                  </form>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
