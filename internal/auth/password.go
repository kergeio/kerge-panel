package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the minimum parameters the panel uses;
// hashes with other parameters are upgraded on the next successful login.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// maxConcurrentHashes bounds the memory spent on concurrent hashing
	// (each hash allocates argonMemoryKiB).
	maxConcurrentHashes = 2
)

// Password length limits, counted in Unicode code points.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
)

var hashSlots = make(chan struct{}, maxConcurrentHashes)

// argonParams are the parameters encoded in a PHC string.
type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

var currentParams = argonParams{memory: argonMemoryKiB, time: argonTime, threads: argonThreads}

func argonKey(password string, salt []byte, p argonParams, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, keyLen)
}

// HashPassword returns an Argon2id hash in PHC string format:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey(password, salt, currentParams, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, currentParams.memory, currentParams.time, currentParams.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var errBadHash = errors.New("auth: malformed password hash")

// VerifyPassword reports whether password matches the PHC-encoded hash, and
// whether the hash should be recomputed with the current parameters.
func VerifyPassword(password, encoded string) (ok, needsRehash bool, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false, errBadHash
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil ||
		p.memory == 0 || p.time == 0 || p.threads == 0 {
		return false, false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, false, errBadHash
	}
	got := argonKey(password, salt, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p != currentParams || len(salt) != argonSaltLen || len(want) != argonKeyLen, nil
}

// dummyHash is verified against when a username does not exist, so that
// the response time does not reveal whether it does.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("dummy password for timing equalization")
	if err != nil {
		panic(err)
	}
	return h
})

// ValidPassword reports whether password satisfies the length rules.
func ValidPassword(password string) bool {
	n := utf8.RuneCountInString(password)
	return utf8.ValidString(password) && n >= MinPasswordLen && n <= MaxPasswordLen
}

// ValidUsername reports whether name is 3-32 characters of letters, digits,
// '.', '_' or '-'.
func ValidUsername(name string) bool {
	if len(name) < 3 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
