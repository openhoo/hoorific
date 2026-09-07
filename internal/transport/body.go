package transport

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"hoorific/internal/core"
)

const spoolChunkSize = 64 * 1024

var spoolMagic = [8]byte{'H', 'O', 'O', 'S', 'P', 'O', 'O', 'L'}

type SpoolConfig struct {
	Dir                           string
	MaxTenantBytes, MaxTotalBytes int64
}
type Spool struct {
	cfg    SpoolConfig
	mu     sync.Mutex
	tenant map[string]int64
	total  int64
}
type oneShotBody struct {
	mu             sync.Mutex
	r              io.ReadCloser
	opened, closed bool
}

func NewOneShotBody(r io.ReadCloser) *oneShotBody { return &oneShotBody{r: r} }
func (b *oneShotBody) Open(context.Context) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("body closed")
	}
	if b.opened {
		return nil, errors.New("body is not replayable")
	}
	b.opened = true
	return b.r, nil
}
func (*oneShotBody) Replayable() bool { return false }
func (b *oneShotBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.r.Close()
}

type spoolBody struct {
	spool                 *Spool
	path, tenant, request string
	key                   [32]byte
	base                  [12]byte
	size                  int64
	mu                    sync.Mutex
	closed                bool
}

func NewSpool(cfg SpoolConfig) (*Spool, error) {
	if cfg.Dir == "" || cfg.MaxTenantBytes <= 0 || cfg.MaxTotalBytes <= 0 {
		return nil, errors.New("invalid spool configuration")
	}
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	return &Spool{cfg: cfg, tenant: make(map[string]int64)}, nil
}

// Capture fully owns and closes no caller reader. On success the returned body owns an encrypted file.
func (s *Spool) Capture(ctx context.Context, tenant, request string, r io.Reader, maxBytes int64) (core.Body, error) {
	if tenant == "" || request == "" || maxBytes <= 0 {
		return nil, errors.New("invalid spool identity or limit")
	}
	if maxBytes > s.cfg.MaxTenantBytes {
		maxBytes = s.cfg.MaxTenantBytes
	}
	if maxBytes > s.cfg.MaxTotalBytes {
		maxBytes = s.cfg.MaxTotalBytes
	}
	f, err := os.CreateTemp(s.cfg.Dir, "hoorific-spool-*.spool")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	fail := func(e error, reserved int64) (core.Body, error) {
		f.Close()
		os.Remove(path)
		if reserved > 0 {
			s.release(tenant, reserved)
		}
		return nil, e
	}
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err, 0)
	}
	body := &spoolBody{spool: s, path: path, tenant: tenant, request: request}
	if _, err = f.Write(spoolMagic[:]); err != nil {
		return fail(err, 0)
	}
	if _, err = rand.Read(body.key[:]); err != nil {
		return fail(err, 0)
	}
	if _, err = rand.Read(body.base[:]); err != nil {
		return fail(err, 0)
	}
	if _, err = f.Write([]byte{1}); err != nil {
		return fail(err, 0)
	}
	if _, err = f.Write(body.base[:]); err != nil {
		return fail(err, 0)
	}
	block, err := aes.NewCipher(body.key[:])
	if err != nil {
		return fail(err, 0)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fail(err, 0)
	}
	br := bufio.NewReaderSize(r, spoolChunkSize)
	buf := make([]byte, spoolChunkSize)
	var idx uint64
	for {
		n, readErr := br.Read(buf)
		if n > 0 {
			if err = ctx.Err(); err != nil {
				return fail(err, body.size)
			}
			if body.size+int64(n) > maxBytes {
				return fail(errors.New("body limit exceeded"), body.size)
			}
			if !s.reserve(tenant, int64(n)) {
				return fail(errors.New("spool quota exceeded"), body.size)
			}
			sealed := gcm.Seal(nil, chunkNonce(body.base, idx), buf[:n], chunkAAD(tenant, request, idx, body.size, int64(n)))
			var hdr [16]byte
			binary.BigEndian.PutUint64(hdr[:8], idx)
			binary.BigEndian.PutUint64(hdr[8:], uint64(n))
			if _, err = f.Write(hdr[:]); err == nil {
				_, err = f.Write(sealed)
			}
			if err != nil {
				return fail(err, body.size+int64(n))
			}
			body.size += int64(n)
			idx++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail(readErr, body.size)
		}
	}
	final := make([]byte, 8)
	binary.BigEndian.PutUint64(final, uint64(body.size))
	sealed := gcm.Seal(nil, chunkNonce(body.base, idx), final, []byte(fmt.Sprintf("%s\x00%s\x00final", tenant, request)))
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[:8], idx)
	binary.BigEndian.PutUint64(hdr[8:], 0)
	if _, err = f.Write(hdr[:]); err == nil {
		_, err = f.Write(sealed)
	}
	if err != nil {
		return fail(err, body.size)
	}
	if err = f.Close(); err != nil {
		os.Remove(path)
		s.release(tenant, body.size)
		return nil, err
	}
	return body, nil
}
func (s *Spool) reserve(tenant string, n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || s.tenant[tenant]+n > s.cfg.MaxTenantBytes || s.total+n > s.cfg.MaxTotalBytes {
		return false
	}
	s.tenant[tenant] += n
	s.total += n
	return true
}
func (s *Spool) release(tenant string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant[tenant] -= n
	s.total -= n
	if s.tenant[tenant] <= 0 {
		delete(s.tenant, tenant)
	}
	if s.total < 0 {
		s.total = 0
	}
}
func (b *spoolBody) Open(ctx context.Context) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("body closed")
	}
	f, err := os.Open(b.path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, err
	}
	return &spoolReader{body: b, f: f, base: b.base}, nil
}
func (*spoolBody) Replayable() bool { return true }
func (b *spoolBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	err := os.Remove(b.path)
	b.spool.release(b.tenant, b.size)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

type spoolReader struct {
	body         *spoolBody
	f            *os.File
	base         [12]byte
	gcm          cipher.AEAD
	idx          uint64
	remaining    int64
	done, header bool
	buf          []byte
}

func (r *spoolReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if !r.header {
		var pre [21]byte
		if _, err := io.ReadFull(r.f, pre[:]); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		if string(pre[:8]) != string(spoolMagic[:]) || pre[8] != 1 {
			return 0, errors.New("spool header invalid")
		}
		if copy(r.base[:], pre[9:]) != 12 {
			return 0, errors.New("spool nonce invalid")
		}
		block, err := aes.NewCipher(r.body.key[:])
		if err != nil {
			return 0, err
		}
		r.gcm, err = cipher.NewGCM(block)
		if err != nil {
			return 0, err
		}
		r.header = true
	}
	if len(r.buf) == 0 {
		var hdr [16]byte
		if _, err := io.ReadFull(r.f, hdr[:]); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		idx := binary.BigEndian.Uint64(hdr[:8])
		rawN := binary.BigEndian.Uint64(hdr[8:])
		if idx != r.idx {
			return 0, errors.New("spool chunk order invalid")
		}
		r.idx++
		if rawN == 0 {
			sealed := make([]byte, 8+int64(r.gcm.Overhead()))
			if _, err := io.ReadFull(r.f, sealed); err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			plain, err := r.gcm.Open(nil, chunkNonce(r.base, idx), sealed, []byte(fmt.Sprintf("%s\x00%s\x00final", r.body.tenant, r.body.request)))
			if err != nil {
				return 0, errors.New("spool final authentication failed")
			}
			if len(plain) != 8 || binary.BigEndian.Uint64(plain) != uint64(r.body.size) || r.remaining != r.body.size {
				return 0, errors.New("spool final length mismatch")
			}
			var extra [1]byte
			if n, _ := r.f.Read(extra[:]); n != 0 {
				return 0, errors.New("spool trailing data")
			}
			r.done = true
			return 0, io.EOF
		}
		if rawN > spoolChunkSize {
			return 0, errors.New("spool chunk too large")
		}
		n := int64(rawN)
		sealed := make([]byte, n+int64(r.gcm.Overhead()))
		if _, err := io.ReadFull(r.f, sealed); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		plain, err := r.gcm.Open(nil, chunkNonce(r.base, idx), sealed, chunkAAD(r.body.tenant, r.body.request, idx, r.remaining, n))
		if err != nil {
			return 0, errors.New("spool chunk authentication failed")
		}
		r.buf = plain
		r.remaining += n
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
func (r *spoolReader) Close() error { return r.f.Close() }
func chunkNonce(base [12]byte, idx uint64) []byte {
	n := append([]byte(nil), base[:]...)
	for i := uint(0); i < 8; i++ {
		n[11-i] ^= byte(idx >> (8 * i))
	}
	return n
}
func chunkAAD(tenant, request string, idx uint64, offset, n int64) []byte {
	return []byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", tenant, request, idx, offset, n))
}
func (s *Spool) CleanupOrphans() error {
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(s.cfg.Dir, e.Name())
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		var marker [8]byte
		n, _ := io.ReadFull(f, marker[:])
		f.Close()
		if n == len(marker) && marker == spoolMagic {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
