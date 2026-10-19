package domain

import (
	"path"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func normalizeSearchExtension(ext string) string {
	// Match the name matcher's NFC and full case folding, retaining accents
	// and separators because extensions are compared as complete values.
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(ext)))
}

func normalizeSearchFilters(o ListingOptions) (ListingOptions, error) {
	if len(o.Ext) > 0 {
		extensions := make([]string, 0, len(o.Ext))
		for _, ext := range o.Ext {
			if ext == "" || strings.HasPrefix(ext, ".") {
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
		if len(o.Ext) > 0 {
			name := path.Base(n.Path)
			dot := strings.LastIndexByte(name, '.')
			// A sole leading dot marks a dotfile, not an extension.
			if dot <= 0 || dot == len(name)-1 {
				return false
			}
			ext := normalizeSearchExtension(name[dot+1:])
			i := sort.SearchStrings(o.Ext, ext)
			if i == len(o.Ext) || o.Ext[i] != ext {
				return false
			}
		}
		if o.MinSize != nil && n.Size < *o.MinSize || o.MaxSize != nil && n.Size > *o.MaxSize {
			return false
		}
	}
	return (o.ModifiedAfter == nil || !n.Modified.Before(*o.ModifiedAfter)) &&
		(o.ModifiedBefore == nil || n.Modified.Before(*o.ModifiedBefore))
}
