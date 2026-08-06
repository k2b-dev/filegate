package domain

import (
	"io"
	"os"
)

// Index is the port interface for metadata storage and retrieval.
type Index interface {
	GetEntity(id FileID) (*Entity, error)
	LookupChild(parentID FileID, name string) (*DirEntry, error)
	// ListChildren returns up to limit children of parentID in listing
	// order (directories first, then files, each alphabetical). after
	// is a strict-greater resume position; its zero value starts at
	// the beginning. The cursor entry itself does not need to exist —
	// the implementation seeks past its key position, so pagination
	// survives concurrent deletion of the cursor entry.
	ListChildren(parentID FileID, after ChildCursor, limit int) ([]DirEntry, error)
	ListEntities() ([]Entity, error)
	ForEachEntity(func(Entity) error) error
	GetVersion(fileID FileID, versionID VersionID) (*VersionMeta, error)
	ListVersions(fileID FileID, after VersionID, limit int) ([]VersionMeta, error)
	LatestVersionTimestamp(fileID FileID) (int64, error)
	MarkVersionsDeleted(fileID FileID, deletedAt int64) (int, error)
	// ForEachFileVersions calls fn once per file that has at least one
	// version, with all versions of that file in ascending Timestamp
	// order. Used by the background pruner to apply retention per file
	// without buffering the whole keyspace in memory.
	ForEachFileVersions(fn func(fileID FileID, versions []VersionMeta) error) error
	// LookupByFlatKey returns the file ID stored at (mountName, relPath)
	// in the secondary flat-key index, or ErrNotFound. O(log n).
	LookupByFlatKey(mountName, relPath string) (FileID, error)
	// IterateFlatKeys walks flat-key entries under mountName whose
	// relPath starts with prefix, in lexical order. after (when
	// non-empty) is a strict-greater bound — useful for paginating
	// past a previous cursor. limit caps the number of fn invocations
	// (zero = unlimited). fn returns (continue, error).
	IterateFlatKeys(mountName, prefix, after string, limit int, fn func(relPath string, id FileID) (bool, error)) error
	// LookupMultipartUploadRecord returns the durable multipart-
	// upload record for uploadID, or ErrNotFound. Used by
	// CompleteMultipartUpload's idempotency check: a present record
	// means the Complete already succeeded once and any retry must
	// return its stored result, not re-do the install.
	LookupMultipartUploadRecord(uploadID [16]byte) (*MultipartUploadRecord, error)
	LookupActiveMultipartUpload(uploadID string) (*ActiveMultipartUpload, error)
	ListActiveMultipartUploads(bucket string) ([]ActiveMultipartUpload, error)
	ListActiveMultipartParts(uploadID string) ([]ActiveMultipartPart, error)
	LookupUploadSession(sessionID string) (*UploadSession, error)
	ListUploadSessions(phase UploadSessionPhase) ([]UploadSession, error)
	ListUploadSegments(sessionID string) ([]UploadSegment, error)
	LookupUploadCommitRecord(sessionID string) (*UploadCommitRecord, error)
	Batch(fn func(Batch) error) error
	Close() error
}

// Batch is a write transaction against the index, used for bulk updates.
type Batch interface {
	PutEntity(entity Entity)
	PutChild(parentID FileID, name string, entry DirEntry)
	DelChild(parentID FileID, name string)
	DelEntity(id FileID)
	PutVersion(meta VersionMeta)
	DelVersion(fileID FileID, versionID VersionID)
	// PutFlatKey inserts/overwrites the flat-key entry for a file at
	// (mountName, relPath) → id. relPath has no leading slash and uses
	// "/" as separator. Directories don't get flat-key entries.
	PutFlatKey(mountName, relPath string, id FileID)
	// DelFlatKey removes the flat-key entry at (mountName, relPath).
	// Idempotent — no-op if the key doesn't exist.
	DelFlatKey(mountName, relPath string)
	// DelFlatKeysUnder removes every flat-key entry whose relPath is
	// equal to relPathPrefix or descends from it (i.e. starts with
	// relPathPrefix + "/"). Used by recursive directory deletes.
	// Pass "" for relPathPrefix to wipe an entire mount.
	DelFlatKeysUnder(mountName, relPathPrefix string)
	// ReKeyFlatPrefix moves every flat-key entry from (oldMount,
	// oldPrefix or descendant) to (newMount, newPrefix or descendant)
	// by stripping oldPrefix and prepending newPrefix from each
	// relPath, preserving file IDs. Used by directory rename/move.
	// Inputs must NOT contain a trailing "/"; an empty prefix means
	// "the entire mount".
	ReKeyFlatPrefix(oldMount, oldPrefix, newMount, newPrefix string)
	// PutMultipartUploadRecord stores or overwrites the durable
	// uploadId record. Caller is responsible for batching this
	// alongside the entity update — both must commit atomically
	// for the 2-phase Complete protocol.
	PutMultipartUploadRecord(uploadID [16]byte, record MultipartUploadRecord)
	// DelMultipartUploadRecord removes a stored upload record.
	// Used by AbortMultipartUpload and the cleanup loop after the
	// retention window elapses.
	DelMultipartUploadRecord(uploadID [16]byte)
	PutActiveMultipartUpload(upload ActiveMultipartUpload)
	DelActiveMultipartUpload(uploadID string)
	PutActiveMultipartPart(part ActiveMultipartPart)
	DelActiveMultipartPart(uploadID string, partNumber int)
	DelActiveMultipartParts(uploadID string)
	PutUploadSession(session UploadSession)
	DelUploadSession(sessionID string)
	PutUploadSegment(segment UploadSegment)
	DelUploadSegment(sessionID string, index int)
	DelUploadSegments(sessionID string)
	PutUploadCommitRecord(record UploadCommitRecord)
	DelUploadCommitRecord(sessionID string)
}

// Store is the port interface for filesystem I/O operations.
type Store interface {
	Abs(path string) (string, error)
	Stat(path string) (os.FileInfo, error)
	ReadDir(path string) ([]os.DirEntry, error)
	MkdirAll(path string, perm os.FileMode) error
	RemoveAll(path string) error
	Remove(path string) error
	Rename(oldPath, newPath string) error
	// RenameNoReplace renames oldPath to newPath, failing with an error
	// matching errors.Is(err, os.ErrExist) when newPath already exists —
	// atomically, without the Stat-then-Rename window in which an
	// external writer's file at newPath would be silently clobbered.
	// Backed by renameat2(RENAME_NOREPLACE) on Linux.
	RenameNoReplace(oldPath, newPath string) error
	OpenRead(path string) (io.ReadCloser, error)
	OpenWrite(path string, perm os.FileMode) (io.WriteCloser, error)
	SetID(path string, id FileID) error
	// SetIDIfAbsent assigns an ID only when the path has none, atomically.
	// Returns the ID that ended up on disk and whether this call wrote it,
	// so a caller that loses the race adopts the winner instead of
	// proceeding with an ID nothing else agrees with.
	SetIDIfAbsent(path string, id FileID) (FileID, bool, error)
	GetID(path string) (FileID, error)
	// CloneFile copies srcPath to dstPath using FICLONE when supported
	// (cheap, constant-time) or a byte copy fallback. dstPath must not exist;
	// callers create the parent directory first. Returns true when the
	// reflink fast-path was used. Used by the versioning subsystem to
	// snapshot file bytes without paying double the storage on reflink-capable filesystems.
	CloneFile(srcPath, dstPath string) (reflinked bool, err error)
}
