package mediasource

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testData() []byte {
	b := make([]byte, 3*chunkSize+123)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestHTTPSource(t *testing.T) {
	data := testData()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "x.mkv", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	src, err := Open(context.Background(), srv.URL+"/dir/My%20Movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != int64(len(data)) || src.Name() != "My Movie.mkv" {
		t.Fatalf("size %d name %q", src.Size(), src.Name())
	}

	// A read spanning a chunk boundary.
	buf := make([]byte, 100)
	off := int64(chunkSize - 50)
	if n, err := src.ReadAt(buf, off); n != 100 || err != nil || !bytes.Equal(buf, data[off:off+100]) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	// Cached: re-reading costs no requests.
	before := requests.Load()
	if _, err := src.ReadAt(buf, off); err != nil || requests.Load() != before {
		t.Errorf("cached ReadAt made %d requests, err %v", requests.Load()-before, err)
	}
	// Reading past the end.
	if n, err := src.ReadAt(buf, int64(len(data)-10)); n != 10 || err != io.EOF {
		t.Errorf("ReadAt at end = %d, %v", n, err)
	}

	rc, err := src.OpenRange(context.Background(), 5, chunkSize*2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data[5:5+chunkSize*2]) {
		t.Errorf("OpenRange returned %d bytes, %v", len(got), err)
	}
}

func TestHTTPSourceWithoutRanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "no ranges here")
	}))
	defer srv.Close()
	if _, err := Open(context.Background(), srv.URL+"/x.mkv"); err == nil || !strings.Contains(err.Error(), "byte-range") {
		t.Errorf("Open = %v, want byte-range error", err)
	}
}

func TestFileSource(t *testing.T) {
	data := testData()
	p := filepath.Join(t.TempDir(), "a.mkv")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if IsRemote(p) || !IsRemote("https://nas/a.mkv") {
		t.Fatal("IsRemote misclassifies")
	}
	src, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rc, err := src.OpenRange(context.Background(), 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data[10:30]) || src.Name() != "a.mkv" || src.Size() != int64(len(data)) {
		t.Errorf("file source mismatch")
	}
}
