package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are OWASP's "m=12 MiB, t=3, p=1" option, one of
// its equivalent recommended configurations; it was chosen over the 19–46 MiB
// options because OpenBrain runs under a 64 MB memory budget. Each hash
// records its parameters, so raising them later rehashes passwords on the
// next successful sign-in.
const (
	argonTime    = 3
	argonMemory  = 12 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// hashSlots serialises hashing so at most one 12 MiB working buffer exists.
// Sign-ins take ~20 ms and are rare, so this costs nothing noticeable.
var hashSlots = make(chan struct{}, 1)

func argonKey(password string, salt []byte, t, m uint32, p uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() {
		// The working buffer is garbage now. Collecting it straight away
		// keeps a burst of sign-ins from growing the heap by 12 MiB per
		// attempt (measured: 72 MB peak RSS without this, under a 64 MB budget).
		runtime.GC()
		<-hashSlots
	}()
	return argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
}

// HashPassword returns an Argon2id hash in PHC string format:
// $argon2id$v=19$m=12288,t=3,p=1$<salt>$<hash>.
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	key := argonKey(password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// checkPassword reports whether password matches encoded, and whether the hash
// uses outdated parameters and should be replaced.
func checkPassword(encoded, password string) (ok, rehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false, errors.New("unsupported password hash")
	}
	var version int
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, false, err
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, false, err
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, false, err
	}
	got := argonKey(password, salt, t, m, p, uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	rehash = version != argon2.Version || m != argonMemory || t != argonTime || p != argonThreads
	return ok, rehash, nil
}

// dummyHash is checked against when a username does not exist, so a failed
// sign-in takes the same time whether or not the account exists.
var dummyHash = sync.OnceValue(func() string { return HashPassword("openbrain-dummy-password") })

const (
	minPassword = 8
	maxPassword = 1024 // long enough for any passphrase; bounds hashing input
)

func (v *validator) password(field, pw, username string) {
	switch {
	case len(pw) < minPassword:
		v.fail(field, fmt.Sprintf("must be at least %d characters", minPassword))
	case len(pw) > maxPassword:
		v.fail(field, fmt.Sprintf("must be at most %d bytes", maxPassword))
	case strings.EqualFold(strings.TrimSpace(pw), username):
		v.fail(field, "must not be the same as the username")
	}
}
