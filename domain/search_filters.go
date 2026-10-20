package domain

import (
	"path"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func normalizeSearchExtension(ext string) string {
	for i := 0; i < len(ext); i++ {
		if ext[i] >= 128 {
			// Match the name matcher's NFC and full case folding, retaining accents
			// and separators because extensions are compared as complete values.
			return norm.NFC.String(cases.Fold().String(norm.NFC.String(ext)))
		}
	}
	// ASCII case folding is lowercase; ToLower reuses already lowercase strings.
	return strings.ToLower(ext)
}

func normalizeSearchFilters(o ListingOptions) (ListingOptions, error) {
	if len(o.Ext) > 0 {
		extensions := make([]string, 0, len(o.Ext))
		for _, ext := range o.Ext {
			ext = strings.TrimSpace(ext)
			if ext == "" || strings.Contains(ext, ".") {
				return o, ErrInvalid
			}
			extensions = append(extensions, normalizeSearchExtension(ext))
		}
		sort.Strings(extensions)
		o.Ext = extensions[:0]
		for _, ext := range extensions {
			if len(o.Ext) == 0 || o.Ext[len(o.Ext)-1] != ext {
				o.Ext = append(o.Ext, ext)
			}
		}
	}
	if o.MinSize != nil && *o.MinSize < 0 || o.MaxSize != nil && *o.MaxSize < 0 ||
		o.MinSize != nil && o.MaxSize != nil && *o.MinSize > *o.MaxSize {
		return o, ErrInvalid
	}
	if o.ModifiedAfter != nil {
		t := o.ModifiedAfter.UTC()
		// UTC normalization must retain an RFC 3339 year so the options
		// remain JSON-encodable when binding the cursor fingerprint.
		if t.Year() < 0 || t.Year() > 9999 {
			return o, ErrInvalid
		}
		o.ModifiedAfter = &t
	}
	if o.ModifiedBefore != nil {
		t := o.ModifiedBefore.UTC()
		if t.Year() < 0 || t.Year() > 9999 {
			return o, ErrInvalid
		}
		o.ModifiedBefore = &t
	}
	return o, nil
}

// Apply metadata filters before name normalization, typo or loose matching.
// The same predicate selects indexed nodes and live filesystem observations.
func (o ListingOptions) matchesSearchFilters(n Node) bool {
	if len(o.Ext) > 0 || o.MinSize != nil || o.MaxSize != nil {
		if n.Directory {
			return false
		}
		if !o.matchesSearchExtension(n.Path) {
			return false
		}
		if o.MinSize != nil && n.Size < *o.MinSize || o.MaxSize != nil && n.Size > *o.MaxSize {
			return false
		}
	}
	return (o.ModifiedAfter == nil || !n.Modified.Before(*o.ModifiedAfter)) &&
		(o.ModifiedBefore == nil || n.Modified.Before(*o.ModifiedBefore))
}

// Extension text is in the key, but a matching directory still needs rejecting
// using metadata. A dotted directory is never a file extension match.
func (o ListingOptions) matchesSearchExtension(p string) bool {
	if len(o.Ext) == 0 {
		return true
	}
	name := path.Base(p)
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return false
	}
	ext := normalizeSearchExtension(name[dot+1:])
	i := sort.SearchStrings(o.Ext, ext)
	return i < len(o.Ext) && o.Ext[i] == ext
}
func (o ListingOptions) needsSearchMetadata() bool {
	return o.Type != "all" || len(o.Ext) > 0 || o.MinSize != nil || o.MaxSize != nil || o.ModifiedAfter != nil || o.ModifiedBefore != nil
}
func (o ListingOptions) matchesNode(n Node) bool {
	return !(o.Type == "files" && n.Directory || o.Type == "directories" && !n.Directory) && (!o.search || o.matchesSearchFilters(n))
}
