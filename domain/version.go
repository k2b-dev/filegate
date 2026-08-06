package domain

// VersionID is the stable identity of a single file version. Internally a
// UUIDv7, so the leading bytes carry the creation timestamp and the trailing
// random tail breaks ms-collision ties between parallel snapshots of the
// same file.
type VersionID [16]byte

// IsZero reports whether the VersionID is the zero value.
func (v VersionID) IsZero() bool {
	var zero VersionID
	return v == zero
}

// VersionMeta is the metadata for one frozen state of a file. The bytes
// themselves live in `<mount>/.fg-versions/<file-id>/<version-id>.bin`,
// linked via reflink when supported (cheap) or copied otherwise.
//
// Pinned versions are exempt from automatic pruning until the file is
// deleted and the post-delete grace period expires.
//
// Label is opaque server-side: callers (typically a frontend) may store
// plain text or JSON. The server does not parse or sanitise it.
//
// DeletedAt tracks orphan state. While the source file exists DeletedAt is
// zero. When the file is deleted, all of its versions transition to
// DeletedAt = unix-ms-of-delete; the pruner then applies the post-delete
// grace policy uniformly (including to pinned versions).
type VersionMeta struct {
	VersionID VersionID
	FileID    FileID
	Timestamp int64 // unix milliseconds
	Size      int64
	Mode      uint32
	Pinned    bool
	Label     string
	DeletedAt int64 // 0 while file lives; unix-ms when entered grace
	// MountName is the configured mount this version's blob belongs
	// to. Persisted so the pruner can locate (and remove) the blob
	// after the source file's entity is deleted — ResolveAbsPath
	// no longer works once the parent chain is gone, but the mount
	// path is stable and the per-mount layout (.fg-versions/<id>/
	// <vid>.bin) is deterministic.
	MountName string
}

// IsOrphan reports whether the source file has been deleted and the
// version is now in the post-delete grace window.
func (v VersionMeta) IsOrphan() bool { return v.DeletedAt > 0 }
