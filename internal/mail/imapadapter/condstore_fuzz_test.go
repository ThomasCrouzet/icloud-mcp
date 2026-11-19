package imapadapter

import (
	"strings"
	"testing"
)

// FuzzStoreObservation feeds arbitrary bytes to the wire observer. The
// observer must never panic and must stay bounded. When it records a MODIFIED
// set, that set must survive a second parse with the same UIDs, so a later
// validation cannot disagree with the recorded evidence.
func FuzzStoreObservation(f *testing.F) {
	f.Add([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"), []byte("T2 OK stored"))
	f.Add([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n"), []byte("T2 OK [MODIFIED 7] failed"))
	f.Add([]byte("T2 UID STORE 7:9 (UNCHANGEDSINCE 42) +FLAGS (\\Seen)\r\n"), []byte("T2 OK [MODIFIED 1:9,20] failed"))
	f.Add([]byte("T2 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS (\\Seen)\r\n"), []byte("T2 OK [MODIFIED ,,]"))
	f.Add([]byte("\r\n"), []byte("\r\n"))
	f.Fuzz(func(t *testing.T, command, status []byte) {
		if len(command) > 4096 || len(status) > 4096 {
			return
		}
		observation := &storeObservation{}
		observation.begin()
		observation.noteWrite(command)
		observation.noteStatus(status)
		proof := observation.proof()
		if proof.failed || !proof.observed || !proof.modified {
			return
		}
		if len(proof.uids) == 0 {
			t.Fatal("a MODIFIED proof without a UID set was accepted")
		}
		ranges, ok := parseUIDRanges(joinRanges(proof.uids))
		if !ok || len(ranges) != len(proof.uids) {
			t.Fatalf("MODIFIED set does not round-trip: %+v", proof.uids)
		}
		for index, r := range proof.uids {
			if ranges[index] != r {
				t.Fatalf("MODIFIED set changed on a second parse: %+v", proof.uids)
			}
		}
	})
}

func joinRanges(ranges []uidRange) string {
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r.start == r.stop {
			parts = append(parts, itoaUID(r.start))
			continue
		}
		parts = append(parts, itoaUID(r.start)+":"+itoaUID(r.stop))
	}
	return strings.Join(parts, ",")
}

func itoaUID(uid uint32) string {
	var buffer [10]byte
	index := len(buffer)
	for {
		index--
		buffer[index] = byte('0' + uid%10)
		uid /= 10
		if uid == 0 {
			break
		}
	}
	return string(buffer[index:])
}
