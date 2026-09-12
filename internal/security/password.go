package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

const passwordRounds = 210000

func NewSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// HashPassword derives a 256-bit key using PBKDF2-HMAC-SHA-256. The one-block
// implementation is intentionally local to keep the production dependency set
// limited to the requested SQLite driver plus the Go standard library.
func HashPassword(password string, salt []byte) []byte {
	key := []byte(password)
	var block [4]byte
	binary.BigEndian.PutUint32(block[:], 1)

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(salt)
	_, _ = mac.Write(block[:])
	u := mac.Sum(nil)
	out := append([]byte(nil), u...)

	for i := 1; i < passwordRounds; i++ {
		mac = hmac.New(sha256.New, key)
		_, _ = mac.Write(u)
		u = mac.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

func NewPasswordHash(password string) (salt, hash []byte, err error) {
	if password == "" {
		return nil, nil, errors.New("empty password")
	}
	salt, err = NewSalt()
	if err != nil {
		return nil, nil, err
	}
	return salt, HashPassword(password, salt), nil
}

func VerifyPassword(password string, salt, expected []byte) bool {
	if len(salt) != 16 || len(expected) != sha256.Size {
		return false
	}
	actual := HashPassword(password, salt)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
