package secret

import (
	"bytes"
	"testing"
)

func mustCipher(t *testing.T) *StaticKeyCipher {
	t.Helper()
	c, err := NewStaticKeyCipherFromHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := mustCipher(t)
	plain := []byte("ilink-bot-token-123")
	ct, ver, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ver != currentKeyVersion {
		t.Fatalf("unexpected version %d", ver)
	}
	got, err := c.Decrypt(ct, ver)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypted mismatch: %q", got)
	}
}

func TestEncryptRejectsEmptyPlaintext(t *testing.T) {
	c := mustCipher(t)
	_, _, err := c.Encrypt([]byte{})
	if err == nil {
		t.Fatal("expected error for empty plaintext")
	}
}

func TestDecryptRejectsShortCiphertext(t *testing.T) {
	c := mustCipher(t)
	_, err := c.Decrypt([]byte{0x01, 0x02}, currentKeyVersion)
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c := mustCipher(t)
	ct, ver, _ := c.Encrypt([]byte("secret"))
	ct[len(ct)-1] ^= 0xff
	_, err := c.Decrypt(ct, ver)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestNewStaticKeyCipherRejectsBadLength(t *testing.T) {
	_, err := NewStaticKeyCipher([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestNewStaticKeyCipherFromHexRejectsInvalidHex(t *testing.T) {
	_, err := NewStaticKeyCipherFromHex("not-hex")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}
