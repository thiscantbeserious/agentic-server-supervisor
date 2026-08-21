package dedup

import "testing"

// C6: dedup.Key(component, evidence) = hex(sha256(component + "\n" + EvidenceCore(evidence)))[:16]

func TestEvidenceCore(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "flattens newlines tabs and CR to spaces",
			in:   "line one\nline\ttwo\rline three",
			want: "line one line two line three",
		},
		{
			name: "strips kernel monotonic stamp",
			in:   "[12345.678901] ata3.00: exception Emask 0x0 SAct 0x0",
			want: "ata3.00: exception emask # sact #",
		},
		{
			name: "strips leading syslog stamp",
			in:   "Aug 15 09:41:02 bam zed[2914]: eid=118 class=checksum",
			want: "bam zed[2914]: eid=# class=checksum",
		},
		{
			name: "strips leading ISO stamp",
			in:   "2026-08-15T09:41:02Z bam kernel: something happened",
			want: "bam kernel: something happened",
		},
		{
			name: "ascii-only lowercase, not strings.ToLower",
			in:   "DEVICE ERROR ÄÖÜ STRASSE",
			want: "device error ÄÖÜ strasse",
		},
		{
			name: "hex token replaced with hash",
			in:   "action 0x6 frozen",
			want: "action # frozen",
		},
		{
			name: "numeric token with separators replaced with hash",
			in:   "cksum_errors=1 read_errors=0.0 ratio=1,234:56",
			want: "cksum_errors=# read_errors=# ratio=#",
		},
		{
			name: "rising counter still collapses to same token",
			in:   "cksum_errors=7",
			want: "cksum_errors=#",
		},
		{
			name: "device identifiers survive token scoping",
			in:   "nvme0n1 sda zed[2914]: warning",
			want: "nvme0n1 sda zed[2914]: warning",
		},
		{
			name: "truncates to 200 runes",
			in:   repeat("a ", 150), // 300 runes before truncation
			want: repeatTrunc("a ", 200),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvidenceCore(tc.in)
			if got != tc.want {
				t.Errorf("EvidenceCore(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func repeatTrunc(s string, runes int) string {
	full := repeat(s, 150)
	r := []rune(full)
	return string(r[:runes])
}

func TestKey(t *testing.T) {
	// A rising cksum_errors counter must yield the SAME key (ARCHITECTURE §2.7 ZFS CKSUM benchmark).
	evidence1 := "eid=1841 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1"
	evidence2 := "eid=1842 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=7"

	k1 := Key("zfs", evidence1)
	k2 := Key("zfs", evidence2)

	if k1 != k2 {
		t.Errorf("rising counter should keep the same key: k1=%q k2=%q", k1, k2)
	}
	if len(k1) != 16 {
		t.Errorf("key length = %d, want 16", len(k1))
	}

	// Different component with identical evidence must differ.
	k3 := Key("kernel", evidence1)
	if k3 == k1 {
		t.Errorf("different component must produce a different key")
	}

	// Deterministic.
	k4 := Key("zfs", evidence1)
	if k4 != k1 {
		t.Errorf("Key must be deterministic")
	}
}

func TestKeyRawAlertVector(t *testing.T) {
	// The raw-alert path uses dedup.Key("kernel", entry.Message), priority is not part of the key.
	msg := "ata3.00: exception Emask 0x0 SAct 0x0 SErr 0x0 action 0x6 frozen"
	k := Key("kernel", msg)
	if len(k) != 16 {
		t.Errorf("key length = %d, want 16", len(k))
	}
}
