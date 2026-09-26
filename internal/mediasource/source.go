// Package mediasource gives random and streaming access to a media file that
// lives either on a local (or network-mounted) filesystem or behind an HTTP(S)
// server that supports byte-range requests.
package mediasource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	// chunkSize is the granularity of the small-read cache used for header
	// and index parsing over HTTP.
	chunkSize = 256 << 10
	// chunkCacheEntries bounds that cache (chunkSize × entries bytes).
	chunkCacheEntries = 32
)

// Source is a seekable media file.
type Source interface {
	io.ReaderAt
	// Size is the total length in bytes.
	Size() int64
	// Name is a human-readable name (the file's base name).
	Name() string
	// OpenRange streams length bytes starting at off. It is the efficient
	// path for large sequential reads (one request over HTTP).
	OpenRange(ctx context.Context, off, length int64) (io.ReadCloser, error)
	Close() error
}

// IsRemote reports whether location is an http(s) URL rather than a path.
func IsRemote(location string) bool {
	return strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://")
}

// Open opens a local path or an http(s) URL.
func Open(ctx context.Context, location string) (Source, error) {
	if IsRemote(location) {
		return openHTTP(ctx, location, http.DefaultClient)
	}
	return openFile(location)
}

type fileSource struct {
	f    *os.File
	size int64
	name string
}

func openFile(p string) (*fileSource, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s is a directory", p)
	}
	return &fileSource{f: f, size: st.Size(), name: filepath.Base(p)}, nil
}

func (s *fileSource) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }
func (s *fileSource) Size() int64                             { return s.size }
func (s *fileSource) Name() string                            { return s.name }
func (s *fileSource) Close() error                            { return s.f.Close() }

func (s *fileSource) OpenRange(_ context.Context, off, length int64) (io.ReadCloser, error) {
	return io.NopCloser(io.NewSectionReader(s.f, off, length)), nil
}

// httpSource reads a remote file with Range requests. Small ReadAt calls go
// through a chunk cache so that parsing headers and indexes does not issue a
// request per element.
type httpSource struct {
	client *http.Client
	url    string
	size   int64
	name   string

	mu     sync.Mutex
	chunks map[int64][]byte
	order  []int64 // LRU, oldest first
}

func openHTTP(ctx context.Context, rawURL string, client *http.Client) (*httpSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	s := &httpSource{client: client, url: rawURL, chunks: make(map[int64][]byte)}
	name, err := url.PathUnescape(path.Base(u.Path))
	if err != nil {
		name = path.Base(u.Path)
	}
	s.name = name

	resp, err := s.get(ctx, 0, 1)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	size, err := contentRangeSize(resp.Header.Get("Content-Range"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rawURL, err)
	}
	s.size = size
	return s, nil
}

// contentRangeSize extracts the complete length from "bytes a-b/size".
func contentRangeSize(v string) (int64, error) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 || v[i+1:] == "*" {
		return 0, fmt.Errorf("server did not report a length in Content-Range %q", v)
	}
	return strconv.ParseInt(v[i+1:], 10, 64)
}

func (s *httpSource) get(ctx context.Context, off, length int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil, errors.New("server does not support byte-range requests")
		}
		return nil, fmt.Errorf("GET %s: %s", s.url, resp.Status)
	}
	return resp, nil
}

func (s *httpSource) Size() int64  { return s.size }
func (s *httpSource) Name() string { return s.name }
func (s *httpSource) Close() error { return nil }

func (s *httpSource) OpenRange(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	if off+length > s.size {
		length = s.size - off
	}
	if length <= 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	resp, err := s.get(ctx, off, length)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *httpSource) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= s.size {
			return n, io.EOF
		}
		base := pos - pos%chunkSize
		chunk, err := s.chunk(base)
		if err != nil {
			return n, err
		}
		c := copy(p[n:], chunk[pos-base:])
		if c == 0 {
			return n, io.EOF
		}
		n += c
	}
	return n, nil
}

func (s *httpSource) chunk(base int64) ([]byte, error) {
	s.mu.Lock()
	if c, ok := s.chunks[base]; ok {
		s.touch(base)
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()

	rc, err := s.OpenRange(context.Background(), base, chunkSize)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	c, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.chunks[base]; !ok {
		s.chunks[base] = c
		s.order = append(s.order, base)
		if len(s.order) > chunkCacheEntries {
			delete(s.chunks, s.order[0])
			s.order = s.order[1:]
		}
	}
	return c, nil
}

func (s *httpSource) touch(base int64) {
	for i, b := range s.order {
		if b == base {
			copy(s.order[i:], s.order[i+1:])
			s.order[len(s.order)-1] = base
			return
		}
	}
}
