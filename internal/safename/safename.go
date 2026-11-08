// Package safename holds shared filename sanitization. Three call sites
// (KB note attachments, HTTP note uploads, chat message attachments) used
// to keep private copies that had already drifted apart; a whitelist fix
// now lands in one place.
package safename

import (
	"path"
	"strings"
	"unicode/utf8"
)

// SanitizeFileName collapses raw into a single safe path component:
// base name only, control chars / path separators / drive prefixes /
// quotes / NUL dropped, leading dots trimmed, and length capped at maxLen
// bytes preserving the extension (backing off to a rune boundary so CJK
// names stay valid UTF-8). Returns "" when nothing safe remains.
// maxLen <= 0 defaults to 120.
func SanitizeFileName(raw string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 120
	}
	if raw == "" {
		return ""
	}
	// Normalize Windows separators to / so path.Base reliably extracts
	// the last component regardless of which side of the wire we run on.
	raw = strings.ReplaceAll(raw, `\`, "/")
	raw = path.Base(raw)
	// `path.Base("..") == ".."`; reject explicitly.
	if raw == "." || raw == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < 0x20, r == 0x7f:
			// control char — drop
		case r == '\\', r == '/', r == ':', r == 0:
			// path separator / drive prefix / NUL — drop
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".") // hidden-dotfile prefix is rarely intended
	// Windows reserved device names (CON, NUL, COM1, …, including
	// "CON.txt" variants) pass every filter above; creating them on a
	// Windows host FS either fails or writes the console device. Also
	// drop trailing dots/spaces, which Windows silently strips (a
	// "file." and "file" then collide). The deployment can be a Windows
	// host, so this is not a POSIX-only concern.
	if isWindowsReservedName(out) {
		return ""
	}
	out = strings.TrimRight(out, ". ")
	if len(out) > maxLen {
		// Truncate from the stem so we preserve the extension. Byte-
		// slicing on UTF-8 would chop multi-byte runes (CJK filenames
		// are 3 bytes/char) and yield invalid UTF-8 on disk, so back
		// off to the nearest rune boundary at or below the byte budget.
		ext := path.Ext(out)
		stem := strings.TrimSuffix(out, ext)
		keep := maxLen - len(ext)
		if keep < 1 {
			keep = 1
		}
		if len(stem) > keep {
			for keep > 0 && !utf8.RuneStart(stem[keep]) {
				keep--
			}
			stem = stem[:keep]
		}
		out = stem + ext
	}
	return out
}

// windowsReserved lists device names Windows reserves in ANY directory,
// matched case-insensitively against the stem (before the extension).
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func isWindowsReservedName(name string) bool {
	if name == "" {
		return false
	}
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	return windowsReserved[strings.ToLower(stem)]
}
