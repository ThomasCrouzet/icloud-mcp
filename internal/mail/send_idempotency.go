package mail

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
)

const (
	maxSendIdempotencyEntries = 128
	maxSendIdempotencyKey     = 255
)

type sendFingerprint [sha256.Size]byte

type sendClaim struct {
	fingerprint sendFingerprint
	done        chan struct{}
	result      SendResult
	err         error
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
// own context is cancelled.
func (c *sendIdempotencyCache) claim(ctx context.Context, sender string, input SendInput) (*sendClaim, bool, SendResult, error) {
	key := input.IdempotencyKey
	fingerprint := sendRequestFingerprint(sender, input)
	c.mu.Lock()
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
		return nil, false, SendResult{}, newError(CodeConflict, "idempotency cache is full; use a new process or a different key")
	}
	claim := &sendClaim{fingerprint: fingerprint, done: make(chan struct{})}
	c.entries[key] = claim
	c.mu.Unlock()
	return claim, true, SendResult{}, nil
}

func (c *sendIdempotencyCache) complete(claim *sendClaim, result SendResult, err error) {
	if claim == nil {
		return
	}
	c.mu.Lock()
	claim.result = result
	claim.err = err
	close(claim.done)
	c.mu.Unlock()
}
