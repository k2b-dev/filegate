package s3

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// AWS S3 ListObjectsV2 limits.
const (
	maxKeysCap     = 1000
	defaultMaxKeys = 1000
)

// handleListObjectsV2 implements GET /{bucket}?list-type=2 — the
// modern S3 listing op. Returns a flat list of object metadata for
// the bucket, optionally filtered by prefix + start-after, paginated
// via continuation-token.
//
// Backed by the Pebble flat-key index from M0 (22aahwtm); for each
// matching flat-key we fetch the entity metadata (mtime, size, etag)
// to populate the Contents entries. The flat-key iterator returns
// entries in lexical order, which is exactly what S3 ListObjectsV2
// expects.
//
// Delimiter / CommonPrefixes are NOT implemented here — they land in
// M2 per the dex plan. A request that includes ?delimiter=… is
// rejected with InvalidArgument so clients fail loudly rather than
// silently see no virtual hierarchy.
func (rt *router) handleListObjectsV2(w http.ResponseWriter, r *http.Request, _ *sigV4Result, bucket string) {
	q := r.URL.Query()
	if got := q.Get("list-type"); got != "2" {
		writeError(w, r, errInvalidArgument, "only ListObjectsV2 (list-type=2) is supported", withBucket(bucket))
		return
	}
	delimiter := q.Get("delimiter")
	if v := q.Get("encoding-type"); v != "" && v != "url" {
		writeError(w, r, errInvalidArgument, "encoding-type must be \"url\" if specified", withBucket(bucket))
		return
	}
	encodeURL := q.Get("encoding-type") == "url"
	if v := q.Get("fetch-owner"); v != "" && v != "false" {
		// AWS returns per-object <Owner> when fetch-owner=true.
		// We don't track per-key owners (single-tenant filegate
		// has one principal); reject the request rather than lie
		// about ownership.
		writeError(w, r, errNotImplemented, "fetch-owner is not supported (filegate has no per-object owner model in M1)", withBucket(bucket))
		return
	}

	prefix := q.Get("prefix")
	startAfter := q.Get("start-after")
	contToken := q.Get("continuation-token")

	maxKeys := defaultMaxKeys
	if raw := q.Get("max-keys"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, r, errInvalidArgument, "max-keys must be a non-negative integer", withBucket(bucket))
			return
		}
		if n > maxKeysCap {
			n = maxKeysCap
		}
		maxKeys = n
	}
	// max-keys=0 is technically valid but cannot make progress —
	// no contents means no NextContinuationToken to carry. Return
	// an empty, non-truncated page so clients don't loop.
	if maxKeys == 0 {
		writeListBucketResult(w, listBucketResultV2{
			Name:     bucket,
			Prefix:   maybeURLEncode(prefix, encodeURL),
			MaxKeys:  0,
			KeyCount: 0,
		})
		return
	}

	// Resolve the cursor: continuation-token (when set) wins over
	// start-after — that's the AWS behavior. Both express a
	// "strict-greater than" bound on the relPath ordering.
	cursor := startAfter
	if contToken != "" {
		decoded, err := decodeContinuationToken(contToken)
		if err != nil {
			writeError(w, r, errInvalidArgument, "continuation-token is malformed", withBucket(bucket))
			return
		}
		cursor = decoded
	}

	// We pass limit=0 (unlimited) and stop the iteration ourselves
	// once we've accepted maxKeys+1 RETURNABLE entries (objects OR
	// common-prefix groupings). Counting callbacks instead of
	// returnables (e.g. via iterLimit=maxKeys+1) would mis-detect
	// truncation when an entry is skipped (stale race, non-file,
	// reserved namespace) — IsTruncated could go wrong in either
	// direction. The "+1" peek past the cap is what sets
	// IsTruncated correctly without losing data.
	//
	// When delimiter is set, we virtual-group keys whose first
	// occurrence of the delimiter (after the prefix) falls at the
	// same boundary. Each unique group becomes one CommonPrefixes
	// entry that consumes one MaxKeys slot — same budget as a
	// regular Contents entry per AWS spec.
	contents := make([]listObjectXML, 0, maxKeys)
	commonPrefixes := make([]commonPrefixXML, 0)
	prefixSet := make(map[string]struct{})
	truncated := false
	totalReturnable := func() int { return len(contents) + len(commonPrefixes) }

	err := rt.svc.IterateFlatKeysForS3(bucket, prefix, cursor, 0, func(relPath string, id domain.FileID) (bool, error) {
		// Filter out reserved-namespace and out-of-subset keys
		// FIRST — they should never surface as either Contents
		// or CommonPrefixes, even if something rogue ended up
		// indexed.
		if validateObjectKey(relPath) != nil {
			return true, nil
		}

		// Compute whether this key rolls up into a CommonPrefix
		// (delimiter set + key has a delimiter after the prefix).
		// AWS-style grouping: take everything from the start of
		// the key up to and including the FIRST occurrence of
		// the delimiter that's strictly after the prefix.
		commonPrefix, isGrouping := computeCommonPrefix(relPath, prefix, delimiter)

		// AWS filters CommonPrefixes by start-after / cursor too:
		// a group whose prefix is <= the cursor must not re-emit.
		// Without this, paginating with delimiter could repeat a
		// group that the previous page already emitted.
		if isGrouping && cursor != "" && commonPrefix <= cursor {
			return true, nil
		}

		if totalReturnable() >= maxKeys {
			// Truncation peek. Detect a real next entry —
			// either a new common-prefix grouping we haven't
			// emitted, or a returnable object key.
			if isGrouping {
				if _, seen := prefixSet[commonPrefix]; seen {
					// Already-emitted group — not a real
					// new returnable, keep walking.
					return true, nil
				}
				truncated = true
				return false, nil
			}
			if !isReturnable(rt.svc, relPath, id) {
				return true, nil
			}
			truncated = true
			return false, nil
		}

		if isGrouping {
			if _, seen := prefixSet[commonPrefix]; seen {
				// Already emitted this group; skip without
				// counting toward the budget.
				return true, nil
			}
			prefixSet[commonPrefix] = struct{}{}
			commonPrefixes = append(commonPrefixes, commonPrefixXML{
				Prefix: maybeURLEncode(commonPrefix, encodeURL),
			})
			return true, nil
		}

		// Fetch entity for size + mtime + etag. A missing entity
		// for a flat-key entry is a transient race (the rescan
		// sweep will clean it up); skip it rather than abort the
		// listing.
		meta, err := rt.svc.GetFile(id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return true, nil
			}
			return false, err
		}
		if meta.Type != "file" {
			return true, nil
		}
		view, _ := rt.svc.GetS3Metadata(id)
		contents = append(contents, listObjectXML{
			Key:          maybeURLEncode(relPath, encodeURL),
			LastModified: time.UnixMilli(meta.Mtime).UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         quoteETag(effectiveETag(meta, view)),
			Size:         meta.Size,
			StorageClass: "STANDARD",
		})
		return true, nil
	})
	if err != nil {
		writeError(w, r, errInternalError, fmt.Sprintf("listing failed: %s", err), withBucket(bucket))
		return
	}

	res := listBucketResultV2{
		Name:           bucket,
		Prefix:         maybeURLEncode(prefix, encodeURL),
		Delimiter:      maybeURLEncode(delimiter, encodeURL),
		StartAfter:     maybeURLEncode(startAfter, encodeURL),
		KeyCount:       len(contents) + len(commonPrefixes),
		MaxKeys:        maxKeys,
		IsTruncated:    truncated,
		Contents:       contents,
		CommonPrefixes: commonPrefixes,
	}
	if encodeURL {
		res.EncodingType = "url"
	}
	if contToken != "" {
		res.ContinuationToken = contToken
	}
	if truncated {
		// Cursor for the next page is the LAST RAW relPath we
		// accepted as a returnable. For a Contents entry that's
		// the last entry's key; for a CommonPrefix that's the
		// first key under that prefix that we encountered (via
		// emittedAt — but a simpler-and-correct rule is to use
		// the prefix string itself as the strict-greater bound,
		// since any later key not-under-this-prefix sorts after
		// the prefix lexically). We pick the bigger of the two
		// to ensure forward progress regardless of which type
		// was last.
		next := lastRawKey(contents, encodeURL)
		if len(commonPrefixes) > 0 {
			lastPrefix := commonPrefixes[len(commonPrefixes)-1].Prefix
			if encodeURL {
				if dec, ok := percentDecode(lastPrefix); ok {
					lastPrefix = dec
				}
			}
			// Resume position for a common-prefix group: any key
			// strictly greater than `prefix + 0xFF*` will be
			// outside the group (at the lexical boundary). The
			// simplest deterministic cursor is the prefix
			// itself plus a high-byte suffix; we use the iterator's
			// strict-greater semantic to advance past every key
			// in the group.
			candidate := lastPrefix + "\xff"
			if candidate > next {
				next = candidate
			}
		}
		res.NextContinuationToken = encodeContinuationToken(next)
	}
	writeListBucketResult(w, res)
}

// computeCommonPrefix returns (group, ok). When delimiter is set
// and relPath has the delimiter past prefix, group is the
// "<prefix><leading-segment><delimiter>" string the AWS spec emits
// in CommonPrefixes; ok=true. Otherwise returns ("", false) —
// caller treats relPath as a regular Contents entry.
func computeCommonPrefix(relPath, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return "", false
	}
	if !strings.HasPrefix(relPath, prefix) {
		// Iterator is prefix-bounded so this shouldn't happen,
		// but defend.
		return "", false
	}
	tail := relPath[len(prefix):]
	idx := strings.Index(tail, delimiter)
	if idx < 0 {
		return "", false
	}
	return prefix + tail[:idx+len(delimiter)], true
}

// isReturnable mirrors the post-fetch filter in the main loop. Used
// during truncation-peek to decide whether the (maxKeys+1)-th
// callback represents a "real next item" worth signalling
// IsTruncated for. Mirrors the validateObjectKey + GetFile checks
// the main loop runs.
func isReturnable(svc *domain.Service, relPath string, id domain.FileID) bool {
	if validateObjectKey(relPath) != nil {
		return false
	}
	meta, err := svc.GetFile(id)
	if err != nil || meta == nil {
		return false
	}
	return meta.Type == "file"
}

// maybeURLEncode percent-encodes s when the listing was requested
// with encoding-type=url. Per AWS spec we encode the contents that
// might contain XML-unsafe bytes; clients opt in.
func maybeURLEncode(s string, enabled bool) string {
	if !enabled {
		return s
	}
	return uriEncode(s, true)
}

// lastRawKey extracts the raw relPath of the last contents entry,
// undoing the url-encoding we may have applied for response shape.
// Used to emit a NextContinuationToken that the iterator can use as
// a strict-greater bound.
func lastRawKey(contents []listObjectXML, encoded bool) string {
	if len(contents) == 0 {
		return ""
	}
	last := contents[len(contents)-1].Key
	if !encoded {
		return last
	}
	dec, ok := percentDecode(last)
	if !ok {
		return last
	}
	return dec
}

// encodeContinuationToken / decodeContinuationToken render the
// internal cursor (the relPath where the next page should begin) as
// an opaque string the client passes back unmodified. Base64-URL
// without padding keeps the token URL-safe.
func encodeContinuationToken(relPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(relPath))
}

func decodeContinuationToken(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// IterateFlatKeysForS3 forwards to the Service-layer iterator. We
// add this as a thin domain method instead of poking idx directly
// from the adapter: the flat-key keyspace is filegate-internal, the
// adapter only sees what the service exposes.
//
// Lives in service_s3_write.go alongside GetS3Metadata for cohesion.
