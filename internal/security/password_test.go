package security

import "testing"

func TestPasswordHash(t *testing.T) {
	salt, hash, err := NewPasswordHash("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct-horse", salt, hash) {
		t.Fatal("password should verify")
	}
	if VerifyPassword("wrong-password", salt, hash) {
		t.Fatal("wrong password verified")
	}
}
