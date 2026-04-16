package blob

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestLocalBlobStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bs, err := NewLocalBlobStore(dir)
	if err != nil {
		t.Fatalf("NewLocalBlobStore: %v", err)
	}
	ctx := context.Background()
	key := "sha256abc"
	data := []byte("hello blob world")

	// Exists before Put → false.
	ok, err := bs.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Fatal("expected blob to not exist before Put")
	}

	// Put.
	if err := bs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Exists after Put → true.
	ok, err = bs.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists after Put: %v", err)
	}
	if !ok {
		t.Fatal("expected blob to exist after Put")
	}

	// Get.
	rc, err := bs.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}

	// Delete.
	if err := bs.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Exists after Delete → false.
	ok, err = bs.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	}
	if ok {
		t.Fatal("expected blob to not exist after Delete")
	}

	// Delete of non-existent key is not an error.
	if err := bs.Delete(ctx, "does-not-exist"); err != nil {
		t.Fatalf("Delete non-existent: %v", err)
	}

	// Verify the file is actually gone from disk.
	if _, statErr := os.Stat(bs.keyPath(key)); !os.IsNotExist(statErr) {
		t.Errorf("expected file to be deleted from disk")
	}
}
