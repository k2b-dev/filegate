package domain

import (
	"context"
	"io"
	"os"
	"path"
	"strings"
)

// ArchiveStat reads filesystem metadata without assigning an indexed identity.
func (r *Root) ArchiveStat(p string) (os.FileInfo, error) {
	f, e := r.archiveOpen(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return f.Stat()
}

func (r *Root) archiveOpen(p string) (*os.File, error) {
	p, e := CleanPath(p)
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return nil, e
	}
	return r.Files.Open(p, os.O_RDONLY, 0)
}

// WalkArchive visits a selected object and, only for a selected directory, its
// descendants. Directory reads are batched; scanBudget counts every encountered
// object, including private entries, and may be shared across selections. Visit
// must separately enforce the output entry and byte budgets.
// Private metadata is omitted, while symlinks and special files fail closed.
func (r *Root) WalkArchive(ctx context.Context, p string, directory bool, maxDepth int, scanBudget *int, visit func(string, os.FileInfo) error) error {
	p, e := CleanPath(p)
	if e != nil {
		return e
	}
	if scanBudget == nil {
		return ErrInvalid
	}
	if *scanBudget <= 0 {
		return ErrLimit
	}
	*scanBudget -= 1
	var walk func(string, bool, int) error
	walk = func(p string, wantDir bool, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if depth > maxDepth {
			return ErrLimit
		}
		f, e := r.archiveOpen(p)
		if e != nil {
			return e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return e
		}
		if st.IsDir() != wantDir || (!st.IsDir() && !st.Mode().IsRegular()) {
			return ErrConflict
		}
		if e = visit(p, st); e != nil {
			return e
		}
		if !st.IsDir() {
			return nil
		}
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			entries, e := f.ReadDir(64)
			for _, entry := range entries {
				if *scanBudget <= 0 {
					return ErrLimit
				}
				*scanBudget -= 1
				name := entry.Name()
				if name == ".filegate" || strings.HasPrefix(name, ".fg-") {
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return ErrInvalid
				}
				if e := walk(path.Join(p, name), entry.IsDir(), depth+1); e != nil {
					return e
				}
			}
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
		}
	}
	return walk(p, directory, 0)
}
