// Package dedup implements the single dedup-key algorithm (C6). Every
// component that needs a finding identity imports Key/EvidenceCore from
// here; nobody recomputes it.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	monotonicStampRe = regexp.MustCompile(`\[\s*[0-9]+\.[0-9]+\]`)
	syslogStampRe    = regexp.MustCompile(`^[A-Za-z]{3}\s+[0-9]{1,2}\s+[0-9]{2}:[0-9]{2}:[0-9]{2}\s+`)
	isoStampRe       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z\s+`)
	hexTokenRe       = regexp.MustCompile(`^0x[0-9a-f]+$`)
	numTokenRe       = regexp.MustCompile(`^[0-9]+([.,:][0-9]+)*$`)
)

// Key is the one dedup identity for the whole system:
// hex(sha256(component + "\n" + EvidenceCore(evidence)))[:16].
func Key(component, evidence string) string {
	sum := sha256.Sum256([]byte(component + "\n" + EvidenceCore(evidence)))
	return hex.EncodeToString(sum[:])[:16]
}

// EvidenceCore normalizes an evidence/log-line string per C6 so that
// non-identity noise (timestamps, counters, event ids) does not fragment
// the dedup key while device/unit identifiers ("nvme0n1", "zed[2914]:")
// survive.
//
// Per C6, masking is "=" -scoped, not whole-field: each field is split on
// "=", the token regexes applied to each part, then rejoined with "=" —
// this is what lets a rising "cksum_errors=1" -> "cksum_errors=7" (or a
// changing "eid=" digit) collapse to the same key while identifiers with
// no "=" (e.g. "nvme0n1") survive untouched.
func EvidenceCore(evidence string) string {
	s := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, evidence)

	s = monotonicStampRe.ReplaceAllString(s, " ")
	s = syslogStampRe.ReplaceAllString(s, "")
	s = isoStampRe.ReplaceAllString(s, "")

	s = asciiLower(s)

	fields := strings.Fields(s)
	for i, f := range fields {
		parts := strings.Split(f, "=")
		for j, p := range parts {
			if hexTokenRe.MatchString(p) || numTokenRe.MatchString(p) {
				parts[j] = "#"
			}
		}
		fields[i] = strings.Join(parts, "=")
	}

	out := strings.Join(fields, " ")

	r := []rune(out)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

// asciiLower lowercases ASCII letters only, leaving every other rune
// (including non-ASCII letters) untouched — C6 requires this instead of
// strings.ToLower, which would also fold non-ASCII case.
func asciiLower(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		out = append(out, r)
	}
	return string(out)
}
