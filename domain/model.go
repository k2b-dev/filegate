// Package domain implements root-scoped file operations and version history.
package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid argument")
	ErrConflict = errors.New("conflict")
	ErrDisabled = errors.New("feature disabled")
	ErrLimit    = errors.New("limit exceeded")
)

const MetadataLimit = 8192

type Metadata map[string]any

func (m *Metadata) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v map[string]any
	if e := d.Decode(&v); e != nil {
		return e
	}
	*m = v
	return nil
}

type Ownership struct {
	UID     *int   `json:"uid,omitempty"`
	GID     *int   `json:"gid,omitempty"`
	Mode    string `json:"mode,omitempty"`
	DirMode string `json:"dirMode,omitempty"`
}
type WriteOptions struct {
	OnConflict string     `json:"onConflict,omitempty"`
	Ownership  *Ownership `json:"ownership,omitempty"`
	Metadata   Metadata   `json:"metadata,omitempty"`
}
type Node struct {
	Root      string    `json:"root"`
	Path      string    `json:"path"`
	ID        string    `json:"id,omitempty"`
	Directory bool      `json:"directory"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Mode      string    `json:"mode"`
	UID       uint32    `json:"uid"`
	GID       uint32    `json:"gid"`
}
type Page struct {
	Items []Node `json:"items"`
	Next  string `json:"next,omitempty"`
}
type Keep struct {
	Last    int `json:"last" yaml:"last"`
	Hourly  int `json:"hourly" yaml:"hourly"`
	Daily   int `json:"daily" yaml:"daily"`
	Weekly  int `json:"weekly" yaml:"weekly"`
	Monthly int `json:"monthly" yaml:"monthly"`
}
type Versioning struct {
	Enabled  bool          `json:"enabled" yaml:"enabled"`
	Cooldown time.Duration `json:"-" yaml:"-"`
	Keep     Keep          `json:"keep" yaml:"keep"`
}
type RootConfig struct {
	Name       string     `json:"name"`
	Path       string     `json:"-"`
	Index      bool       `json:"index"`
	Versioning Versioning `json:"versioning"`
}
type Version struct {
	ID       string    `json:"id"`
	FileID   string    `json:"fileId"`
	Created  time.Time `json:"created"`
	Size     int64     `json:"size"`
	Pinned   bool      `json:"pinned"`
	Metadata Metadata  `json:"metadata,omitempty"`
	CopyMode string    `json:"copyMode"`
}
type Stats struct {
	Files       int64     `json:"files"`
	Directories int64     `json:"directories"`
	Bytes       int64     `json:"bytes"`
	Updated     time.Time `json:"updated"`
	Source      string    `json:"source"`
}
type IndexStatus struct {
	Enabled    bool       `json:"enabled"`
	Rebuilding bool       `json:"rebuilding"`
	Scanned    int64      `json:"scanned"`
	LastBuilt  *time.Time `json:"lastBuilt"`
	DurationMS int64      `json:"durationMs"`
	Error      string     `json:"error,omitempty"`
}
type RootInfo struct {
	Name          string      `json:"name"`
	Index         IndexStatus `json:"index"`
	Stats         *Stats      `json:"stats"`
	Versioning    Versioning  `json:"versioning"`
	Cooldown      string      `json:"cooldown"`
	Versions      int64       `json:"versions"`
	VersionBytes  int64       `json:"versionBytes"`
	Filesystem    string      `json:"filesystem"`
	Capacity      uint64      `json:"capacity"`
	Available     uint64      `json:"available"`
	ActiveUploads int64       `json:"activeUploads"`
	StagingBytes  int64       `json:"stagingBytes"`
}
type Change struct {
	Key    string
	Value  []byte
	Delete bool
}

// State keeps separate key families for derived index rows and durable records.
type State interface {
	Get(string, any) error
	Put(string, any) error
	Delete(string) error
	Batch([]Change) error
	Scan(string, func(string, []byte) error) error
	Close() error
}

// Files only opens beneath its root, rejecting symbolic links in every component.
type Files interface {
	SecurePrivate() error
	Open(string, int, os.FileMode) (*os.File, error)
	Stat(string) (os.FileInfo, error)
	Mkdir(string, os.FileMode) error
	Rename(string, string, bool) error
	Remove(string, bool) error
	Sync(string) error
	ID(*os.File) (string, error)
	SetID(*os.File, string) error
	Identity(os.FileInfo) (uint64, uint64, uint32, uint32, uint64)
	Clone(*os.File, *os.File) (bool, error)
	Capacity() (string, uint64, uint64, error)
	Close() error
}

func encoded(key string, v any) (Change, error) {
	b, e := json.Marshal(v)
	return Change{Key: key, Value: b}, e
}
func ValidateMetadata(m Metadata) error {
	b, e := json.Marshal(m)
	if e != nil {
		return ErrInvalid
	}
	if len(b) > MetadataLimit {
		return ErrLimit
	}
	return nil
}
func copyStream(dst io.Writer, src io.Reader, max int64) (int64, error) {
	limit := max
	if max < int64(^uint64(0)>>1) {
		limit++
	}
	n, e := io.Copy(dst, io.LimitReader(src, limit))
	if e == nil && n > max {
		e = ErrLimit
	}
	return n, e
}
