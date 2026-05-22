package templates

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbertschler/squirrel/config"
)

// humanBytes formats a byte count for the directory listing — base-1024
// because directory sizes are read by users next to ls/df output, which
// uniformly use KiB/MiB/GiB even when labelled "K"/"M"/"G". Returns
// "-" for zero so an empty row reads as "no data" rather than "0 B".
func humanBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), suffix)
}

// joinStrings is the template-friendly form of strings.Join. Empty slices
// render as "—" so an unconfigured field stands out from a single-entry
// one. Kept named so the templ generator picks it up as a func call.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, sep)
}

// sortedDestinationNames returns destination names in a stable order so
// the volumes page renders the same row sequence on every request. Map
// iteration order in Go is randomised.
func sortedDestinationNames(m map[string]*config.Destination) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// destEndpoint returns a short, user-recognisable rendering of where the
// destination points. The exact param key differs per type (sftp uses
// host, s3 uses bucket, etc.); this picks the most identifying one.
func destEndpoint(d *config.Destination) string {
	if d == nil {
		return ""
	}
	switch d.Type {
	case "sftp":
		if h := d.Params["host"]; h != "" {
			if u := d.Params["user"]; u != "" {
				return u + "@" + h
			}
			return h
		}
	case "s3":
		if b := d.Params["bucket"]; b != "" {
			return "s3://" + b
		}
	case "b2":
		if b := d.Params["bucket"]; b != "" {
			return "b2://" + b
		}
	case "gcs":
		if b := d.Params["bucket"]; b != "" {
			return "gcs://" + b
		}
	case "local":
		return ""
	}
	return d.Type
}

// shortHash truncates a hex BLAKE3 digest for inline display.
func shortHash(hex string) string { return ShortHash(hex) }

// ShortHash is the exported variant used by handlers that need to derive
// a page title from a digest.
func ShortHash(hex string) string {
	if len(hex) <= 12 {
		return hex
	}
	return hex[:12] + "…"
}

// barWidth maps (part / total) onto a CSS percentage width, clamped to
// [1, 100] so even tiny entries get a visible sliver and a saturated
// entry doesn't overflow its container.
func barWidth(part, total int64) string {
	if total <= 0 || part <= 0 {
		return "0%"
	}
	pct := float64(part) * 100.0 / float64(total)
	if pct > 100 {
		pct = 100
	}
	if pct < 1 {
		pct = 1
	}
	return fmt.Sprintf("%.1f%%", pct)
}
