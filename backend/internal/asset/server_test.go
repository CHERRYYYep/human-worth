package asset

import (
	"testing"
)

func TestAssetGuards(t *testing.T) {
	if !bytesEqual([]byte("same"), []byte("same")) || bytesEqual([]byte("same"), []byte("different")) {
		t.Fatal("unexpected byte comparison")
	}
	if err := storageReady(&Server{}); err == nil {
		t.Fatal("missing storage accepted")
	}
	if _, _, err := decodeAssetCursor("not-a-cursor", "acct_alice"); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}
