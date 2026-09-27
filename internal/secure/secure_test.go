package secure

import "testing"

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyPassword("correct horse battery", hash) {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword("wrong password", hash) {
		t.Fatal("expected wrong password to fail")
	}
	if VerifyPassword("correct horse battery", "garbage") {
		t.Fatal("expected malformed hash to fail")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	ct, err := Encrypt(key, []byte("sk-secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == "sk-secret" {
		t.Fatal("ciphertext must not equal plaintext")
	}
	pt, err := Decrypt(key, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(pt) != "sk-secret" {
		t.Fatalf("got %q", pt)
	}
}

func TestAPIKeyGeneration(t *testing.T) {
	k, err := NewAPIKey()
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if len(k.Full) < 20 || k.Prefix == "" || k.Hash != HashToken(k.Full) {
		t.Fatalf("unexpected key: %+v", k)
	}
	k2, _ := NewAPIKey()
	if k.Hash == k2.Hash {
		t.Fatal("keys must be unique")
	}
}

func TestMask(t *testing.T) {
	if Mask("") != "" {
		t.Fatal("empty mask")
	}
	if got := Mask("sk-1234567890"); got == "sk-1234567890" {
		t.Fatalf("mask leaked secret: %s", got)
	}
}
