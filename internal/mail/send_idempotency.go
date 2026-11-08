package mail

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

const (
	maxSendIdempotencyEntries = 128
	maxSendIdempotencyKey     = 255
	// sendIdempotencyDefinitiveRetention bounds duplicate protection for
	// completed definitive outcomes. Pending and ambiguous outcomes do not
	// expire during the process lifetime.
	sendIdempotencyDefinitiveRetention = 15 * time.Minute
)

type sendFingerprint [sha256.Size]byte

type sendClaim struct {
	fingerprint sendFingerprint
	done        chan struct{}
	result      SendResult
	err         error
	completed   bool
	expires     time.Time
}

type sendIdempotencyCache struct {
	mu      sync.Mutex
	entries map[string]*sendClaim
}

func newSendIdempotencyCache() *sendIdempotencyCache {
	return &sendIdempotencyCache{entries: make(map[string]*sendClaim)}
}

func validateSendIdempotencyKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > maxSendIdempotencyKey {
		return validationError("idempotency_key exceeds its byte limit")
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return validationError("idempotency_key must not contain CR, LF, or NUL")
	}
	return nil
}

func sendRequestFingerprint(sender string, input SendInput) sendFingerprint {
	hash := sha256.New()
	write := func(value string) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write(sender)
	for _, group := range [][]string{input.To, input.Cc, input.Bcc} {
		write(string(rune(len(group))))
		for _, address := range group {
			write(strings.ToLower(strings.TrimSpace(address)))
		}
	}
	write(input.Subject)
	write(input.Body)
	write(input.InReplyTo)
	for _, reference := range input.References {
		write(reference)
	}
	if input.ArchiveSent {
		write("archive_sent=true")
	} else {
		write("archive_sent=false")
	}
	var result sendFingerprint
	copy(result[:], hash.Sum(nil))
	return result
}

// claim returns whether the caller owns this key and must dispatch the send.
// A duplicate waits for the owner without releasing the claim, even when its
// own context is cancelled. Expired definitive claims are reclaimed lazily.
func (c *sendIdempotencyCache) claim(ctx context.Context, sender string, input SendInput, now time.Time) (*sendClaim, bool, SendResult, error) {
	key := input.IdempotencyKey
	fingerprint := sendRequestFingerprint(sender, input)
	c.mu.Lock()
	c.expireLocked(now)
	if existing, ok := c.entries[key]; ok {
		c.mu.Unlock()
		if existing.fingerprint != fingerprint {
			return nil, false, SendResult{}, newError(CodeConflict, "idempotency_key was already used with different content")
		}
		select {
		case <-ctx.Done():
			return nil, false, SendResult{}, contextError()
		case <-existing.done:
			return nil, false, existing.result, existing.err
		}
	}
	if len(c.entries) >= maxSendIdempotencyEntries {
		c.mu.Unlock()
		return nil, false, SendResult{}, sendIdempotencyFullError()
	}
	claim := &sendClaim{fingerprint: fingerprint, done: make(chan struct{})}
	c.entries[key] = claim
	c.mu.Unlock()
	return claim, true, SendResult{}, nil
}

func (c *sendIdempotencyCache) complete(claim *sendClaim, result SendResult, err error, completedAt time.Time) {
	if claim == nil {
		return
	}
	c.mu.Lock()
	claim.result = result
	claim.err = err
	claim.completed = true
	if !completedAt.IsZero() && sendClaimIsDefinitive(result, err) {
		claim.expires = completedAt.Add(sendIdempotencyDefinitiveRetention)
	}
	close(claim.done)
	c.mu.Unlock()
}

// expireLocked deletes only completed definitive claims at or past their
// completion-based deadline. Pending and ambiguous claims never expire here.
// A clock rollback leaves claims retained because their deadline is still in
// the future.
func (c *sendIdempotencyCache) expireLocked(now time.Time) {
	if now.IsZero() {
		return
	}
	for key, claim := range c.entries {
		if !claim.completed || claim.expires.IsZero() {
			continue
		}
		if !claim.expires.After(now) {
			delete(c.entries, key)
		}
	}
}

// sendClaimIsDefinitive reports whether a completed claim may be reclaimed
// after the definitive retention window. Ambiguous outcomes, and any result the
// send boundary did not classify explicitly, stay protected until process exit.
func sendClaimIsDefinitive(result SendResult, err error) bool {
	public := AsError(err)
	if result.Status == SendOutcomeUnknown || (public != nil && public.Code == CodeOutcomeUnknown) {
		return false
	}
	if result.Status == SendAccepted && err == nil {
		return true
	}
	if public == nil {
		return false
	}
	switch result.Status {
	case SendRejected:
		return definitiveSendErrorCode(public.Code)
	case "":
		return preDispatchSendErrorCode(public.Code)
	default:
		return false
	}
}

func definitiveSendErrorCode(code Code) bool {
	switch code {
	case CodeValidation, CodeAuthorization, CodeRateLimited, CodeTimeout,
		CodeUnavailable, CodeAuthentication, CodeProtocolError, CodePayloadTooLarge,
		CodeNotFound, CodeInternalError:
		return true
	}
	return false
}

func preDispatchSendErrorCode(code Code) bool {
	switch code {
	case CodeValidation, CodeAuthorization, CodeRateLimited, CodeTimeout:
		return true
	}
	return false
}

func sendIdempotencyFullError() *Error {
	return &Error{
		Code:    CodeConflict,
		Message: "send idempotency capacity is full; no SMTP submission was dispatched for this key",
		Reconciliation: "Keep the same key and unchanged parameters. Existing keys can still return " +
			"their retained results. Capacity recovers only when retained definitive outcomes expire. " +
			"Pending or uncertain outcomes stay until process exit. Do not restart or use a " +
			"different key to retry an uncertain send. Check Sent and confirm with recipients " +
			"before another send.",
	}
}
