package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

const conflictAttempts = 8

// Random suffixes keep repeated duplication constant-cost; finding the next
// sequential integer would rescan every earlier duplicate on every request.
func conflictName(requested string, directory bool) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	suffix := "-" + hex.EncodeToString(random[:])
	name := path.Base(requested)
	extension := ""
	if !directory {
		extension = path.Ext(name)
	}
	stem := strings.TrimSuffix(name, extension)
	// Linux names are bounded in bytes, not runes. Reserve at least one stem
	// byte; unusually long extensions are shortened without breaking UTF-8.
	extension = trimNameBytes(extension, 255-len(suffix)-1)
	stem = trimNameBytes(stem, 255-len(suffix)-len(extension))
	if stem == "" {
		stem = "f"
	}
	return path.Join(path.Dir(requested), stem+suffix+extension), nil
}
func trimNameBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// chooseTarget never assigns IDs, updates an index or reads file content.
// Its absence observation is advisory; publication still uses NOREPLACE.
func (r *Root) chooseTarget(requested string, directory bool, policy string) (string, bool, error) {
	if policy != "" && policy != "error" && policy != "overwrite" && policy != "rename" {
		return "", false, ErrInvalid
	}
	st, err := r.Files.Stat(requested)
	if errors.Is(err, os.ErrNotExist) {
		return requested, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if policy == "overwrite" {
		if directory || !st.Mode().IsRegular() {
			return "", false, ErrConflict
		}
		return requested, true, nil
	}
	if policy != "rename" {
		return "", false, ErrConflict
	}
	for i := 0; i < conflictAttempts; i++ {
		candidate, err := conflictName(requested, directory)
		if err != nil {
			return "", false, err
		}
		if _, err = r.Files.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, false, nil
		}
		if err != nil {
			return "", false, err
		}
	}
	return "", false, fmt.Errorf("%w: no free conflict target after %d attempts", ErrLimit, conflictAttempts)
}

// renamePublication keeps each attempted destination durable before invoking the
// filesystem. Recovery can therefore recognize the published inode after a lost
// syscall response or process crash, including a collision retry.
func (r *Root) renamePublication(key string, rec *publication, replace bool, requested, policy string) error {
	for attempt := 0; attempt < conflictAttempts; attempt++ {
		err := r.Files.Rename(rec.Temp, rec.Path, replace)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// A retried NFS NOREPLACE can report EEXIST after the first attempt was
		// applied. Do not abandon that publication by changing its journal path.
		if st, e := r.Files.Stat(rec.Path); e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == rec.Claim.Device && ino == rec.Claim.Inode {
				// The failed syscall path did not run the normal rename
				// durability barriers. Keep the intent until both succeed.
				if e := r.Files.Sync(path.Dir(rec.Path)); e != nil {
					return e
				}
				return r.Files.Sync(path.Dir(rec.Temp))
			}
		}
		if policy != "rename" {
			return err
		}
		if attempt == conflictAttempts-1 {
			return fmt.Errorf("%w: publication collided %d times", ErrLimit, conflictAttempts)
		}
		st, e := r.Files.Stat(rec.Temp)
		if e != nil {
			return err
		}
		dev, ino, _, _, _ := r.Files.Identity(st)
		if dev != rec.Claim.Device || ino != rec.Claim.Inode {
			return ErrConflict
		}
		candidate, e := conflictName(requested, rec.Node.Directory)
		if e != nil {
			return e
		}
		rec.Path, rec.Node.Path, rec.Claim.Path = candidate, candidate, candidate
		if rec.Receipt != nil {
			node := rec.Node
			rec.Receipt.Result = &node
		}
		if e = r.State.Put(key, *rec); e != nil {
			return e
		}
		replace = false
	}
	return ErrLimit
}
