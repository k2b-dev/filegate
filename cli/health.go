package cli

import (
	"errors"
	"fmt"
	"log"

	"github.com/valentinkolb/filegate/infra/filesystem"
)

// minSafeFreeBytes is the threshold below which the startup
// health check warns. Operators who run that low have probably
// run out of headroom for any non-trivial upload; the daemon
// keeps starting (the upload code path enforces its own
// upload.min_free_bytes per request) but the warning surfaces
// the looming problem.
const minSafeFreeBytes uint64 = 1 << 30 // 1 GiB

// checkMountsHealthOrFail probes every mount via
// filesystem.CheckMountHealth and:
//
//   - WARN-logs free-space below minSafeFreeBytes (informational).
//   - INFO-logs the per-mount summary unconditionally (operators
//     get a fingerprint of every mount the daemon will use).
//   - HARD-FAILS startup when any mount has Errors. Hard-fail is
//     the correct policy: a missing-xattr mount silently drops
//     fileIDs, a read-only mount can't accept any PUT, a missing
//     mount path produces 500s on every request to that bucket.
//     Fast feedback at startup beats slow debugging at 3am.
//
// The health check happens before the index opens — the index
// rebuild is expensive and pointless if the underlying storage
// is broken. An error returned here aborts the serve command
// before any goroutines are spawned.
func checkMountsHealthOrFail(paths []string) ([]filesystem.MountHealth, error) {
	if len(paths) == 0 {
		return nil, errors.New("storage.base_paths is empty — at least one mount is required")
	}
	results := filesystem.CheckMountsHealth(paths)
	var bad []string
	for _, h := range results {
		freeGiB := float64(h.FreeBytes) / (1 << 30)
		totalGiB := float64(h.TotalBytes) / (1 << 30)
		if len(h.Errors) > 0 {
			log.Printf("[filegate] startup health: FAIL %s — %s", h.Path, joinErrs(h.Errors))
			bad = append(bad, fmt.Sprintf("%s: %s", h.Path, joinErrs(h.Errors)))
			continue
		}
		copyMode := "byte-copy"
		if h.ReflinkSupported {
			copyMode = "reflink"
		}
		log.Printf("[filegate] startup health: OK %s — writable, xattr-supported, copy=%s, free=%.1f GiB / %.1f GiB",
			h.Path, copyMode, freeGiB, totalGiB)
		if h.FreeBytes > 0 && h.FreeBytes < minSafeFreeBytes {
			log.Printf("[filegate] WARN %s has only %.1f GiB free (< %.1f GiB threshold) — uploads may start failing soon",
				h.Path, freeGiB, float64(minSafeFreeBytes)/(1<<30))
		}
	}
	if len(bad) > 0 {
		return results, fmt.Errorf("startup health: %d mount(s) failed: %s", len(bad), joinErrs(bad))
	}
	return results, nil
}

// joinErrs flattens a slice of error strings into a single
// human-readable line. Used for both per-mount and aggregated
// startup-failure messages.
func joinErrs(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	out := errs[0]
	for _, e := range errs[1:] {
		out += "; " + e
	}
	return out
}
