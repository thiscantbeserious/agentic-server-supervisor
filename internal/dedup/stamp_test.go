package dedup

import "testing"

func TestEvidenceCoreStampOrdering(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"syslog then monotonic",
			"Aug 15 20:20:14 [ 1234.567890] ata3.00: exception Emask 0x0",
			"Aug 15 21:31:02 [ 9999.111111] ata3.00: exception Emask 0x0"},
		{"monotonic then syslog",
			"[ 1234.567890] Aug 15 20:20:14 ata3.00: exception",
			"[ 9999.111111] Aug 16 03:02:01 ata3.00: exception"},
		{"monotonic then ISO",
			"[ 1234.567890] 2026-08-15T20:20:14Z ata3.00: exception",
			"[ 9999.111111] 2026-08-16T03:02:01Z ata3.00: exception"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if Key("kernel", c.a) != Key("kernel", c.b) {
				t.Errorf("keys differ\n a core=%q\n b core=%q", EvidenceCore(c.a), EvidenceCore(c.b))
			}
		})
	}
}
