package imapadapter

import (
	"strings"
	"testing"
)

// TestStoreObservationBindsOnlyItsOwnTag proves that the observer records the
// tagged status of the STORE command it sent. A tagged status for another
// command, an untagged status, and a nested response are never evidence.
func TestStoreObservationBindsOnlyItsOwnTag(t *testing.T) {
	for name, test := range map[string]struct {
		inbound []string
		want    storeProof
	}{
		"own tagged ok without modified": {
			inbound: []string{"* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged) MODSEQ (43))", "T2 OK stored"},
			want:    storeProof{observed: true, ok: true},
		},
		"own tagged ok with modified": {
			inbound: []string{"T2 OK [MODIFIED 7] Conditional STORE failed"},
			want:    storeProof{observed: true, ok: true, modified: true, uids: []uidRange{{start: 7, stop: 7}}},
		},
		"own tagged no is observed without ok": {
			inbound: []string{"T2 NO guard rejected"},
			want:    storeProof{observed: true},
		},
		"only another command tag": {
			inbound: []string{"T1 OK [MODIFIED 7] other command"},
			want:    storeProof{failed: true},
		},
		"only an untagged status": {
			inbound: []string{"* OK [MODIFIED 7] untagged"},
			want:    storeProof{failed: true},
		},
		"tag prefix does not match": {
			inbound: []string{"T20 OK [MODIFIED 7] longer tag"},
			want:    storeProof{failed: true},
		},
		"no status at all": {
			inbound: []string{"* 1 FETCH (UID 7 FLAGS (\\Seen))"},
			want:    storeProof{failed: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			observation := &storeObservation{}
			observation.begin()
			observation.noteWrite([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"))
			for _, line := range test.inbound {
				observation.noteStatus([]byte(line))
			}
			got := observation.proof()
			if got.observed != test.want.observed || got.ok != test.want.ok || got.modified != test.want.modified || got.failed != test.want.failed {
				t.Fatalf("proof() = %+v, want %+v", got, test.want)
			}
			if test.want.uids != nil && !got.covers(7) {
				t.Fatalf("proof() lost the MODIFIED set: %+v", got)
			}
		})
	}
}

// TestStoreObservationRefusesUnbindableCommand proves that the observer fails
// closed when it cannot bind a tag: two STORE commands, a STORE without the
// precondition, and an oversized command line.
func TestStoreObservationRefusesUnbindableCommand(t *testing.T) {
	for name, commands := range map[string][]string{
		"two guarded stores": {
			"T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n",
			"T3 UID STORE 8 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n",
		},
		"store without precondition": {"T2 UID STORE 7 +FLAGS.SILENT (\\Flagged)\r\n"},
		"unusable tag atom":          {"bad tag UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"},
		"oversized store line":       {"T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (" + strings.Repeat("x", maxObservedCommandLine) + ")\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			observation := &storeObservation{}
			observation.begin()
			for _, command := range commands {
				observation.noteWrite([]byte(command))
			}
			if !observation.proof().failed {
				t.Fatal("an unbindable STORE command produced a usable proof")
			}
		})
	}
}

// TestStoreObservationIgnoresUnrelatedCommands proves that the observer reads
// the STORE command of a session that also sends other commands.
func TestStoreObservationIgnoresUnrelatedCommands(t *testing.T) {
	observation := &storeObservation{}
	observation.begin()
	observation.noteWrite([]byte("T1 UID FETCH 7 (UID FLAGS MODSEQ)\r\n"))
	observation.noteStatus([]byte("T1 OK fetched"))
	observation.noteWrite([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"))
	observation.noteStatus([]byte("T1 OK [MODIFIED 7] stale command"))
	observation.noteStatus([]byte("T2 OK stored"))
	proof := observation.proof()
	if !proof.observed || !proof.ok || proof.modified || proof.failed {
		t.Fatalf("proof() = %+v", proof)
	}
}

// TestStoreObservationUnparsableStatusFails proves that a MODIFIED set which
// cannot be parsed is not accepted as "not covering this UID".
func TestStoreObservationUnparsableStatusFails(t *testing.T) {
	for name, line := range map[string]string{
		"unterminated code":   "T2 OK [MODIFIED 7 stored",
		"empty range":         "T2 OK [MODIFIED ,] stored",
		"overflowing uid":     "T2 OK [MODIFIED 99999999999] stored",
		"too many ranges":     "T2 OK [MODIFIED " + strings.TrimSuffix(strings.Repeat("7,", maxObservedModifiedRanges+2), ",") + "] stored",
		"unknown status type": "T2 FAILED stored",
		"oversized line":      "T2 OK " + strings.Repeat("x", maxObservedStatusLine),
	} {
		t.Run(name, func(t *testing.T) {
			observation := &storeObservation{}
			observation.begin()
			observation.noteWrite([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"))
			observation.noteStatus([]byte(line))
			if !observation.proof().failed {
				t.Fatalf("unparsable status %q produced a usable proof", line)
			}
		})
	}
}

// TestStoreObservationIsIdleWhenDisarmed proves that an unarmed observation
// records nothing, so every other command of the session pays no parse.
func TestStoreObservationIsIdleWhenDisarmed(t *testing.T) {
	observation := &storeObservation{}
	observation.noteWrite([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"))
	observation.noteStatus([]byte("T2 OK [MODIFIED 7] failed"))
	if !observation.proof().failed {
		t.Fatal("a disarmed observation produced evidence")
	}
	observation.begin()
	observation.disarm()
	observation.noteWrite([]byte("T3 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"))
	observation.noteStatus([]byte("T3 OK stored"))
	if !observation.proof().failed {
		t.Fatal("a disarmed observation recorded a tagged status")
	}
}

func TestParseUIDRangesAcceptsBoundedSets(t *testing.T) {
	ranges, ok := parseUIDRanges("7")
	if !ok || len(ranges) != 1 || !ranges[0].contains(7) || ranges[0].contains(8) {
		t.Fatalf("single uid = %+v, %v", ranges, ok)
	}
	ranges, ok = parseUIDRanges("7,9:11")
	if !ok || len(ranges) != 2 || !ranges[1].contains(9) || ranges[1].contains(12) {
		t.Fatalf("range set = %+v, %v", ranges, ok)
	}
	for _, value := range []string{"", " ", "0", "8,7", "9:8", "7:7:7", "-1", "1e3", "4294967296"} {
		if _, ok := parseUIDRanges(value); ok {
			t.Fatalf("parseUIDRanges(%q) accepted an unsafe set", value)
		}
	}
}

func TestStoreProofCoversRange(t *testing.T) {
	proof := storeProof{uids: []uidRange{{start: 100, stop: 150}}}
	if !proof.covers(101) || proof.covers(99) || proof.covers(151) {
		t.Fatal("MODIFIED range cover is wrong")
	}
	if (storeProof{}).covers(1) {
		t.Fatal("an empty MODIFIED set covered a UID")
	}
}
