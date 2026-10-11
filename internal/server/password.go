package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. These are reasonable interactive-login defaults; they
// are encoded into the hash so they can be tuned later without breaking
// existing hashes.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns an argon2id PHC-format encoded hash of pw.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

const (
	// maxConcurrentHashes bounds argon2 work in flight: each hash takes
	// argonMemory (64 MiB), so a burst of sign-in attempts was a memory
	// exhaustion attack.
	maxConcurrentHashes = 2
	// defaultHashWait is how long a password check waits for a free slot
	// before the request is refused as busy.
	defaultHashWait = 10 * time.Second
)

// errHashBusy means no argon2 slot freed up in time.
var errHashBusy = errors.New("too many password checks in progress")

// withHashSlot runs fn, which hashes or verifies a password, once one of the
// maxConcurrentHashes slots is free. It returns errHashBusy, without running
// fn, when no slot frees within s.hashWait or ctx ends first.
func (s *Server) withHashSlot(ctx context.Context, fn func()) error {
	t := time.NewTimer(s.hashWait)
	defer t.Stop()
	select {
	case s.hashSlots <- struct{}{}:
	case <-t.C:
		return errHashBusy
	case <-ctx.Done():
		return errHashBusy
	}
	defer func() { <-s.hashSlots }()
	fn()
	return nil
}

// VerifyPassword reports whether pw matches the encoded argon2id hash. It uses a
// constant-time comparison. A malformed hash returns an error.
func VerifyPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("malformed argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("parse version: %w", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("parse params: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}

	got := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
