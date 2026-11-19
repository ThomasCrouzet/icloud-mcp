package imapadapter

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	// One IMAP command line is short. A longer outbound line cannot be a
	// command this observer binds, so the observation fails closed.
	maxObservedCommandLine = 512
	// A tagged status line carries one bounded response code. The guard
	// already bounds every protocol line; this bound keeps the parse small.
	maxObservedStatusLine = 4096
	// One single-UID guarded STORE expects at most a few MODIFIED ranges.
	maxObservedModifiedRanges = 64
	maxObservedTagLength      = 32
)

// uidRange is one inclusive range of a MODIFIED message set.
type uidRange struct{ start, stop uint32 }

func (r uidRange) contains(uid uint32) bool { return uid >= r.start && uid <= r.stop }

// storeProof is the tagged-status evidence for one guarded STORE. Only a
// parsed tagged status for the tag of this command is evidence. Every other
// input leaves the proof failed and the caller must fail closed.
type storeProof struct {
	observed bool
	ok       bool
	modified bool
	uids     []uidRange
	failed   bool
}

func (p storeProof) covers(uid uint32) bool {
	for _, r := range p.uids {
		if r.contains(uid) {
			return true
		}
	}
	return false
}

// storeObservation records the tagged status of one STORE command.
//
// go-imap v2 beta.9 discards a tagged OK response code, so the adapter cannot
// read the MODIFIED response code through the client API. The guard sees every
// raw byte of the session, so it records the status line of exactly one
// command. The tag comes from the outbound STORE command line, never from a
// guess, so a response for another command is never evidence.
//
// One mutex protects the fields because the guard read and write paths run on
// different goroutines. The armed flag is atomic so an idle session pays one
// load per protocol line and no lock.
type storeObservation struct {
	armed atomic.Bool
	mu    sync.Mutex

	failed bool
	tag    string
	stores int

	commandLine []byte
	observed    bool
	ok          bool
	modified    bool
	uids        []uidRange
}

// begin arms the observation and clears earlier evidence.
func (o *storeObservation) begin() {
	o.armed.Store(false)
	o.mu.Lock()
	o.failed, o.tag, o.stores = false, "", 0
	o.observed, o.ok, o.modified, o.uids = false, false, false, nil
	o.commandLine = o.commandLine[:0]
	o.mu.Unlock()
	o.armed.Store(true)
}

// disarm stops the observation. The evidence stays readable until begin.
func (o *storeObservation) disarm() { o.armed.Store(false) }

// noteFailLocked marks the observation unusable. Nothing later clears it.
func (o *storeObservation) noteFailLocked() {
	o.failed = true
	o.commandLine = o.commandLine[:0]
}

// noteWrite parses the outbound STORE command line of this session.
func (o *storeObservation) noteWrite(p []byte) {
	if !o.armed.Load() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.armed.Load() || o.failed {
		return
	}
	for _, b := range p {
		o.commandLine = append(o.commandLine, b)
		if b != '\n' {
			if len(o.commandLine) > maxObservedCommandLine {
				o.noteFailLocked()
				return
			}
			continue
		}
		line := o.commandLine[:len(o.commandLine)-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		o.commandLine = o.commandLine[:0]
		o.noteCommandLocked(line)
		if o.failed {
			return
		}
	}
}

// noteCommandLocked binds the tag of the guarded STORE command. A second
// STORE command in one observation window is ambiguous and fails closed.
func (o *storeObservation) noteCommandLocked(line []byte) {
	fields := strings.Fields(string(line))
	if !isStoreCommand(fields) {
		return
	}
	o.stores++
	tag := fields[0]
	if o.stores > 1 || o.tag != "" && o.tag != tag || !validObservedTag(tag) {
		o.noteFailLocked()
		return
	}
	if !strings.Contains(strings.ToUpper(string(line)), "UNCHANGEDSINCE") {
		o.noteFailLocked()
		return
	}
	o.tag = tag
}

func isStoreCommand(fields []string) bool {
	if len(fields) >= 3 && strings.EqualFold(fields[1], "UID") {
		return strings.EqualFold(fields[2], "STORE")
	}
	return len(fields) >= 2 && strings.EqualFold(fields[1], "STORE")
}

// noteStatus reads one completed inbound protocol line. Only a tagged status
// for the bound tag is evidence. An untagged line or another command's tagged
// line is ignored, never guessed at.
func (o *storeObservation) noteStatus(line []byte) {
	if !o.armed.Load() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.armed.Load() || o.failed || o.tag == "" {
		return
	}
	if len(line) > maxObservedStatusLine {
		o.noteFailLocked()
		return
	}
	tag, rest, ok := cutAtom(line)
	if !ok || tag != o.tag {
		return
	}
	status, rest, ok := cutAtom(rest)
	if !ok {
		o.noteFailLocked()
		return
	}
	if o.observed {
		// A second tagged status for one command is a protocol violation.
		o.noteFailLocked()
		return
	}
	o.observed = true
	switch strings.ToUpper(status) {
	case "OK":
		o.ok = true
	case "NO", "BAD":
	default:
		o.noteFailLocked()
		return
	}
	code, value, present, malformed := parseResponseCode(rest)
	if malformed {
		o.noteFailLocked()
		return
	}
	if !present || !strings.EqualFold(code, "MODIFIED") {
		return
	}
	ranges, parsed := parseUIDRanges(value)
	if !parsed {
		o.noteFailLocked()
		return
	}
	o.modified = true
	o.uids = ranges
}

// proof copies the evidence. An observation without one bound guarded STORE
// and one tagged status for that tag proves nothing.
func (o *storeObservation) proof() storeProof {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed || o.tag == "" || o.stores != 1 || !o.observed {
		return storeProof{failed: true}
	}
	return storeProof{observed: true, ok: o.ok, modified: o.modified, uids: append([]uidRange(nil), o.uids...)}
}

func validObservedTag(tag string) bool {
	if tag == "" || len(tag) > maxObservedTagLength {
		return false
	}
	for i := 0; i < len(tag); i++ {
		b := tag[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '.', b == '_':
		default:
			return false
		}
	}
	return true
}

// cutAtom reads one space-delimited atom. It rejects an empty atom and any
// non-printable byte, and skips leading spaces.
func cutAtom(line []byte) (string, []byte, bool) {
	line = bytes.TrimLeft(line, " ")
	end := bytes.IndexByte(line, ' ')
	if end < 0 {
		end = len(line)
	}
	if end == 0 {
		return "", nil, false
	}
	atom := line[:end]
	for _, b := range atom {
		if b <= ' ' || b == 0x7f {
			return "", nil, false
		}
	}
	if end == len(line) {
		return string(atom), nil, true
	}
	return string(atom), line[end+1:], true
}

// parseResponseCode reads the leading resp-text-code of a tagged status text.
// A code that does not parse is reported as malformed, so the caller fails
// closed instead of guessing.
func parseResponseCode(rest []byte) (code, value string, present, malformed bool) {
	rest = bytes.TrimLeft(rest, " ")
	if len(rest) == 0 || rest[0] != '[' {
		return "", "", false, false
	}
	end := bytes.IndexByte(rest, ']')
	if end < 0 {
		return "", "", false, true
	}
	code, tail, ok := cutAtom(rest[1:end])
	if !ok {
		return "", "", false, true
	}
	return code, string(bytes.TrimLeft(tail, " ")), true, false
}

// parseUIDRanges parses a MODIFIED message set. It accepts only ascending,
// non-zero, bounded numeric ranges.
func parseUIDRanges(value string) ([]uidRange, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedStatusLine {
		return nil, false
	}
	parts := strings.Split(value, ",")
	if len(parts) > maxObservedModifiedRanges {
		return nil, false
	}
	out := make([]uidRange, 0, len(parts))
	var previous uint32
	for _, part := range parts {
		start, stop, ok := parseUIDRange(part)
		if !ok || start == 0 || stop < start || len(out) > 0 && start <= previous {
			return nil, false
		}
		out = append(out, uidRange{start: start, stop: stop})
		previous = stop
	}
	return out, true
}

func parseUIDRange(part string) (uint32, uint32, bool) {
	first, second, ranged := strings.Cut(strings.TrimSpace(part), ":")
	start, ok := parseBoundedUID(first)
	if !ok {
		return 0, 0, false
	}
	if !ranged {
		return start, start, true
	}
	stop, ok := parseBoundedUID(second)
	if !ok {
		return 0, 0, false
	}
	return start, stop, true
}

// parseBoundedUID accepts 1 to 10 decimal digits and rejects overflow.
func parseBoundedUID(value string) (uint32, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 10 {
		return 0, false
	}
	var out uint64
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b < '0' || b > '9' {
			return 0, false
		}
		out = out*10 + uint64(b-'0')
	}
	if out > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(out), true
}

// validateConditionalStore decides the outcome of one guarded single-UID
// STORE. The tagged status is the only proof that the UNCHANGEDSINCE
// precondition ran, so success needs both the tagged OK without MODIFIED for
// this UID and the FETCH of that UID with the requested delta. A MODSEQ
// increase, an unsolicited FETCH, or a missing FETCH never prove success.
func validateConditionalStore(uid uint32, add bool, flags []string, proof storeProof, buffers []*imapclient.FetchMessageBuffer) (Message, error) {
	if proof.failed || !proof.observed {
		// The command may have run, but the wire does not show its outcome.
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	if proof.modified {
		if proof.covers(uid) {
			// The UNCHANGEDSINCE test failed for this UID and nothing changed.
			return Message{}, &Error{Kind: ErrorConflict}
		}
		// A single-UID STORE lists only the UIDs that failed the test.
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	if !proof.ok {
		// The tagged status was NO or BAD.
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	// The tagged OK carried no MODIFIED code, so the precondition passed for
	// this UID. RFC 7162 3.1.3 requires one untagged FETCH with MODSEQ for the
	// stored message. Without it the command ran but its final state is
	// unverified, so the outcome stays unknown instead of a silent success.
	if len(buffers) == 0 {
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	if len(buffers) != 1 || buffers[0].UID != imap.UID(uid) {
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	buffer := buffers[0]
	if buffer.ModSeq == 0 {
		return Message{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	message := Message{UID: uid, ModSeq: buffer.ModSeq}
	for _, flag := range buffer.Flags {
		message.Flags = append(message.Flags, string(flag))
	}
	if !conditionalDeltaMatches(buffer.Flags, add, flags) {
		// The precondition passed but the returned flags contradict the
		// requested delta, so another writer changed the message.
		return Message{}, &Error{Kind: ErrorConflict}
	}
	return message, nil
}

// conditionalDeltaMatches compares flags without case, because IMAP flags are
// case-insensitive.
func conditionalDeltaMatches(actual []imap.Flag, add bool, requested []string) bool {
	present := make(map[string]bool, len(actual))
	for _, flag := range actual {
		present[strings.ToLower(string(flag))] = true
	}
	for _, flag := range requested {
		if add != present[strings.ToLower(flag)] {
			return false
		}
	}
	return true
}
