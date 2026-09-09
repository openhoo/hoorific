package transport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	nativeProtocol        = 1
	nativeProfile         = "codex-exec"
	nativeVersion         = "0.153.4"
	nativeMaxFrame        = 64 << 10
	nativeMaxBody         = 32 << 20
	nativeStartupTimeout  = 15 * time.Second
	nativeHeaderTimeout   = 120 * time.Second
	nativeShutdownTimeout = 5 * time.Second
	nativeSocketName      = "wire.sock"
)

var (
	errNativeResponseTruncated = errors.New("native response stream truncated")
	errNativeStreamFailure     = errors.New("native response stream failed")
)

type nativeStartup struct {
	Protocol int    `json:"protocol"`
	Profile  string `json:"profile"`
	Version  string `json:"version"`
	OpenSSL  string `json:"openssl"`
}

type nativeEngine struct {
	path string

	mu       sync.Mutex
	started  bool
	closed   bool
	startErr error
	dir      string
	socket   string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	waitDone chan struct{}
	conns    map[net.Conn]*nativeResponseBody
}

func (p *Pool) ensureNative(path string) (*nativeEngine, error) {
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("transport pool is closed")
	}
	if p.native == nil {
		p.native = &nativeEngine{path: resolveNativeEnginePath(path), conns: make(map[net.Conn]*nativeResponseBody)}
	}
	engine := p.native
	p.mu.Unlock()
	if err := engine.start(); err != nil {
		return nil, err
	}
	return engine, nil
}

func resolveNativeEnginePath(configured string) string {
	if configured != "" {
		return configured
	}
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), "hoorific-codex-wire")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling
		}
	}
	if path, err := exec.LookPath("hoorific-codex-wire"); err == nil {
		return path
	}
	return "hoorific-codex-wire"
}

func (e *nativeEngine) start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("native helper is closed")
	}
	if e.started {
		if e.startErr == nil {
			select {
			case <-e.waitDone:
				e.startErr = errors.New("native helper has exited")
			default:
			}
		}
		return e.startErr
	}
	e.started = true

	dir, err := os.MkdirTemp("", "hoorific-codex-wire-")
	if err != nil {
		e.startErr = fmt.Errorf("native helper temporary directory: %w", err)
		return e.startErr
	}
	if err = os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		e.startErr = fmt.Errorf("native helper temporary directory permissions: %w", err)
		return e.startErr
	}
	socket := filepath.Join(dir, nativeSocketName)
	cmd := exec.Command(e.path, "--socket", socket)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(dir)
		e.startErr = fmt.Errorf("native helper stdin: %w", err)
		return e.startErr
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.RemoveAll(dir)
		e.startErr = fmt.Errorf("native helper stdout: %w", err)
		return e.startErr
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		_ = os.RemoveAll(dir)
		e.startErr = fmt.Errorf("native helper start: %w", err)
		return e.startErr
	}
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	e.dir, e.socket, e.cmd, e.stdin, e.waitDone = dir, socket, cmd, stdin, waitDone

	startup := make(chan error, 1)
	go func() {
		startup <- readNativeStartup(stdout)
		_ = stdout.Close()
	}()
	timer := time.NewTimer(nativeStartupTimeout)
	defer timer.Stop()
	select {
	case err = <-startup:
	case <-timer.C:
		err = errors.New("native helper startup timeout")
	}
	if err == nil {
		err = waitNativeSocket(socket, waitDone, nativeStartupTimeout)
	}
	if err == nil {
		err = os.Chmod(socket, 0600)
	}
	if err == nil {
		e.startErr = nil
		return nil
	}
	e.startErr = fmt.Errorf("native helper readiness: %w", err)
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	select {
	case <-waitDone:
	case <-time.After(nativeShutdownTimeout):
	}
	_ = os.RemoveAll(dir)
	return e.startErr
}

func readNativeStartup(stdout io.Reader) error {
	reader := bufio.NewReaderSize(stdout, nativeMaxFrame)
	line, err := readNativeLine(reader, nativeMaxFrame)
	if err != nil {
		return err
	}
	var startup nativeStartup
	if err := json.Unmarshal(line, &startup); err != nil {
		return errors.New("native helper startup is not JSON")
	}
	if startup.Protocol != nativeProtocol || startup.Profile != nativeProfile || startup.Version != nativeVersion || !strings.HasPrefix(startup.OpenSSL, "OpenSSL 3.6.3") {
		return errors.New("native helper startup metadata mismatch")
	}
	return nil
}

func readNativeLine(reader *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > max {
			return nil, errors.New("native helper startup line too long")
		}
		if err == nil {
			return bytesTrimSpace(line), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

func waitNativeSocket(path string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-done:
			return errors.New("native helper exited before socket readiness")
		case <-deadline.C:
			return errors.New("native helper socket readiness timeout")
		case <-tick.C:
		}
	}
}

func (e *nativeEngine) open(ctx context.Context) (net.Conn, error) {
	e.mu.Lock()
	if !e.started || e.startErr != nil {
		err := e.startErr
		if err == nil {
			err = errors.New("native helper is not ready")
		}
		e.mu.Unlock()
		return nil, err
	}
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("native helper is closed")
	}
	socket := e.socket
	e.mu.Unlock()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("native helper is closed")
	}
	e.conns[conn] = nil
	e.mu.Unlock()
	return conn, nil
}

func (e *nativeEngine) remove(conn net.Conn) {
	e.mu.Lock()
	delete(e.conns, conn)
	e.mu.Unlock()
}

func (e *nativeEngine) attach(conn net.Conn, body *nativeResponseBody) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	if _, ok := e.conns[conn]; !ok {
		return false
	}
	e.conns[conn] = body
	return true
}
func (e *nativeEngine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	type active struct {
		conn net.Conn
		body *nativeResponseBody
	}
	activeConns := make([]active, 0, len(e.conns))
	for conn, body := range e.conns {
		activeConns = append(activeConns, active{conn: conn, body: body})
	}
	stdin, cmd, waitDone, dir := e.stdin, e.cmd, e.waitDone, e.dir
	e.mu.Unlock()
	for _, item := range activeConns {
		if item.body != nil {
			item.body.abort()
		} else {
			_ = item.conn.Close()
			e.remove(item.conn)
		}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if waitDone != nil {
		select {
		case <-waitDone:
		case <-time.After(nativeShutdownTimeout):
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case <-waitDone:
			case <-time.After(nativeShutdownTimeout):
			}
		}
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	return nil
}

type nativeRequest struct {
	Protocol       int        `json:"protocol"`
	PoolKey        string     `json:"pool_key"`
	Method         string     `json:"method"`
	URL            string     `json:"url"`
	Headers        [][]string `json:"headers"`
	Addresses      []string   `json:"addresses"`
	BodyLength     int64      `json:"body_length"`
	MaxConnections int        `json:"max_connections"`
}

type nativeResponse struct {
	Status  int        `json:"status"`
	Headers [][]string `json:"headers"`
	Error   string     `json:"error"`
}

type nativeRoundTripper struct {
	engine    *nativeEngine
	policy    NetworkPolicy
	poolKey   string
	semaphore chan struct{}
}

func newNativeRoundTripper(engine *nativeEngine, policy NetworkPolicy, cacheKey string, maxConnections int) *nativeRoundTripper {
	if maxConnections <= 0 {
		maxConnections = 1
	}
	digest := sha256.Sum256([]byte(cacheKey))
	return &nativeRoundTripper{engine: engine, policy: policy, poolKey: hex.EncodeToString(digest[:]), semaphore: make(chan struct{}, maxConnections)}
}

func (t *nativeRoundTripper) CloseIdleConnections() {}

func (t *nativeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("native transport requires a URL")
	}
	defer reqBodyClose(req)
	cancelBody := context.AfterFunc(req.Context(), func() { reqBodyClose(req) })
	defer cancelBody()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if err := validNativeURL(req.URL); err != nil {
		return nil, err
	}
	if !validNativeMethod(req.Method) {
		return nil, errors.New("native request method is invalid")
	}
	select {
	case t.semaphore <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-t.semaphore
		}
	}
	body, cleanup, err := prepareNativeBody(req)
	if err != nil {
		release()
		return nil, err
	}
	defer cleanup()
	addresses, err := resolveNativeAddresses(req.Context(), req.URL, t.policy)
	if err != nil {
		release()
		return nil, err
	}
	headerValues := nativeHeaders(req.Header)
	frame := nativeRequest{Protocol: nativeProtocol, PoolKey: t.poolKey, Method: req.Method, URL: req.URL.String(), Headers: headerValues, Addresses: addresses, BodyLength: body.length, MaxConnections: cap(t.semaphore)}
	encoded, err := json.Marshal(frame)
	if err != nil || len(encoded) > nativeMaxFrame {
		release()
		if err == nil {
			err = errors.New("native request header too large")
		}
		return nil, err
	}
	conn, err := t.engine.open(req.Context())
	if err != nil {
		release()
		return nil, err
	}
	var responseBody atomic.Pointer[nativeResponseBody]
	stop := context.AfterFunc(req.Context(), func() {
		if body := responseBody.Load(); body != nil {
			body.abort()
			return
		}
		_ = conn.Close()
	})
	deadline := time.Now().Add(nativeHeaderTimeout)
	if requestDeadline, ok := req.Context().Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	_ = conn.SetDeadline(deadline)
	if err = writeNativeFrame(conn, encoded); err == nil && body.length > 0 {
		err = copyNativeBody(conn, req.Context(), body.reader, body.length)
	}
	if err != nil {
		stop()
		_ = conn.Close()
		t.engine.remove(conn)
		release()
		if contextErr := req.Context().Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, err
	}
	metadata, err := readNativeResponse(conn)
	if err != nil {
		stop()
		_ = conn.Close()
		t.engine.remove(conn)
		release()
		if contextErr := req.Context().Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, io.EOF) {
			return nil, errNativeResponseTruncated
		}
		return nil, err
	}
	if metadata.Status == 0 {
		stop()
		_ = conn.Close()
		t.engine.remove(conn)
		release()
		if metadata.Error == "" {
			return nil, errors.New("native helper request failed")
		}
		return nil, errors.New(metadata.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	responseHeaders := make(http.Header, len(metadata.Headers))
	for _, pair := range metadata.Headers {
		if len(pair) != 2 || pair[0] == "" {
			continue
		}
		responseHeaders.Add(pair[0], pair[1])
	}
	SanitizeResponseHeaders(responseHeaders)
	bodyReader := &nativeResponseBody{conn: conn, engine: t.engine, stop: stop, release: release, ctx: req.Context()}
	responseBody.Store(bodyReader)
	if !t.engine.attach(conn, bodyReader) {
		bodyReader.abort()
		return nil, errors.New("native helper is closed")
	}
	if contextErr := req.Context().Err(); contextErr != nil {
		bodyReader.abort()
		return nil, contextErr
	}
	response := &http.Response{StatusCode: metadata.Status, Status: fmt.Sprintf("%d %s", metadata.Status, http.StatusText(metadata.Status)), Header: responseHeaders, Body: bodyReader, Request: req, ContentLength: -1}
	if value := responseHeaders.Get("Content-Length"); value != "" {
		if length, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && length >= 0 {
			response.ContentLength = length
		}
	}
	return response, nil
}

type nativeBodyPlan struct {
	reader  io.Reader
	length  int64
	cleanup func()
}

func prepareNativeBody(req *http.Request) (nativeBodyPlan, func(), error) {
	if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
		return nativeBodyPlan{reader: strings.NewReader(""), cleanup: func() {}}, func() {}, nil
	}
	if req.ContentLength > nativeMaxBody {
		return nativeBodyPlan{}, func() {}, errors.New("native request body exceeds limit")
	}
	if req.ContentLength >= 0 {
		return nativeBodyPlan{reader: contextReader{ctx: req.Context(), reader: req.Body}, length: req.ContentLength, cleanup: func() {}}, func() {}, nil
	}
	file, err := os.CreateTemp("", "hoorific-native-body-")
	if err != nil {
		return nativeBodyPlan{}, func() {}, err
	}
	_ = os.Chmod(file.Name(), 0600)
	remove := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	limited := io.LimitReader(contextReader{ctx: req.Context(), reader: req.Body}, nativeMaxBody+1)
	count, err := io.Copy(file, limited)
	if err != nil {
		remove()
		return nativeBodyPlan{}, func() {}, err
	}
	if count > nativeMaxBody {
		remove()
		return nativeBodyPlan{}, func() {}, errors.New("native request body exceeds limit")
	}
	if err = file.Close(); err != nil {
		remove()
		return nativeBodyPlan{}, func() {}, err
	}
	reader, err := os.Open(file.Name())
	if err != nil {
		remove()
		return nativeBodyPlan{}, func() {}, err
	}
	cleanup := func() {
		_ = reader.Close()
		_ = os.Remove(file.Name())
	}
	return nativeBodyPlan{reader: reader, length: count, cleanup: cleanup}, cleanup, nil
}

func reqBodyClose(req *http.Request) {
	if req.Body != nil && req.Body != http.NoBody {
		_ = req.Body.Close()
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if n == 0 && err == nil {
		if contextErr := r.ctx.Err(); contextErr != nil {
			return 0, contextErr
		}
	}
	return n, err
}

func copyNativeBody(dst io.Writer, ctx context.Context, src io.Reader, length int64) error {
	written, err := io.CopyN(dst, contextReader{ctx: ctx, reader: src}, length)
	if err != nil {
		return err
	}
	if written != length {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func validNativeURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("native transport requires HTTP or HTTPS")
	}
	if u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("native URL authority is invalid")
	}
	return nil
}

func validNativeMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, r := range method {
		if r <= 0x20 || r >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?={} \t", r) {
			return false
		}
	}
	return true
}

func nativeHeaders(input http.Header) [][]string {
	forbidden := map[string]bool{
		"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
		"cookie": true, "proxy-authorization": true, "proxy-connection": true, "keep-alive": true,
		"te": true, "trailer": true, "upgrade": true,
	}
	pairs := make([][]string, 0, len(input))
	for name, values := range input {
		if forbidden[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			pairs = append(pairs, []string{name, value})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		left, right := strings.ToLower(pairs[i][0]), strings.ToLower(pairs[j][0])
		if left == right {
			return pairs[i][1] < pairs[j][1]
		}
		return left < right
	})
	return pairs
}

func resolveNativeAddresses(ctx context.Context, u *url.URL, policy NetworkPolicy) ([]string, error) {
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" {
		return nil, errors.New("native URL host is empty")
	}
	if len(policy.AllowedHosts) > 0 {
		allowed := false
		for _, candidate := range policy.AllowedHosts {
			if strings.EqualFold(host, strings.TrimSuffix(candidate, ".")) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, errors.New("egress host denied")
		}
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if parsed, err := netip.ParseAddr(host); err == nil {
		return pinNativeAddresses([]netip.Addr{parsed}, port, policy)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("egress DNS returned no addresses")
	}
	return pinNativeAddresses(ips, port, policy)
}

func pinNativeAddresses(ips []netip.Addr, port string, policy NetworkPolicy) ([]string, error) {
	seen := make(map[string]struct{}, len(ips))
	addresses := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = ip.Unmap()
		if !AllowedAddress(ip, policy) {
			return nil, errors.New("egress address denied")
		}
		address := net.JoinHostPort(ip.String(), port)
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, errors.New("egress DNS returned no addresses")
	}
	sort.Strings(addresses)
	return addresses, nil
}

func writeNativeFrame(w io.Writer, payload []byte) error {
	if len(payload) > nativeMaxFrame {
		return errors.New("native frame too large")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if err := writeNativeAll(w, prefix[:]); err != nil {
		return err
	}
	return writeNativeAll(w, payload)
}

func writeNativeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readNativeResponse(conn net.Conn) (nativeResponse, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nativeResponse{}, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length > nativeMaxFrame {
		return nativeResponse{}, errors.New("native response header too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nativeResponse{}, err
	}
	var response nativeResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nativeResponse{}, errors.New("native response header is not JSON")
	}
	return response, nil
}

type nativeResponseBody struct {
	conn       net.Conn
	engine     *nativeEngine
	stop       func() bool
	release    func()
	ctx        context.Context
	finishOnce sync.Once
	aborted    atomic.Bool
	mu         sync.Mutex
	pending    []byte
	ended      bool
}

func (b *nativeResponseBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if b.aborted.Load() {
		return 0, b.abortError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aborted.Load() {
		return 0, b.abortError()
	}
	if b.ended {
		return 0, io.EOF
	}
	if len(b.pending) > 0 {
		n := copy(dst, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	var prefix [4]byte
	if _, err := io.ReadFull(b.conn, prefix[:]); err != nil {
		b.ended = true
		b.finishRead()
		if b.aborted.Load() || (b.ctx != nil && b.ctx.Err() != nil) {
			return 0, b.abortError()
		}
		return 0, errNativeResponseTruncated
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 {
		b.ended = true
		b.finishRead()
		return 0, io.EOF
	}
	if length == ^uint32(0) {
		b.ended = true
		b.finishRead()
		return 0, errNativeStreamFailure
	}
	if length > nativeMaxFrame {
		b.ended = true
		b.finishRead()
		return 0, errors.New("native response chunk too large")
	}
	b.pending = make([]byte, length)
	if _, err := io.ReadFull(b.conn, b.pending); err != nil {
		b.pending = nil
		b.ended = true
		b.finishRead()
		if b.aborted.Load() || (b.ctx != nil && b.ctx.Err() != nil) {
			return 0, b.abortError()
		}
		return 0, errNativeResponseTruncated
	}
	n := copy(dst, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *nativeResponseBody) finishRead() {
	b.finish()
}

func (b *nativeResponseBody) finish() {
	b.finishOnce.Do(func() {
		if b.stop != nil {
			b.stop()
		}
		_ = b.conn.Close()
		if b.engine != nil {
			b.engine.remove(b.conn)
		}
		if b.release != nil {
			b.release()
		}
	})
}

func (b *nativeResponseBody) abortError() error {
	if b.ctx != nil {
		if err := b.ctx.Err(); err != nil {
			return err
		}
	}
	return errNativeResponseTruncated
}

func (b *nativeResponseBody) abort() {
	b.aborted.Store(true)
	b.finish()
}

func (b *nativeResponseBody) Close() error {
	if b.stop != nil {
		b.stop()
	}
	b.finish()
	return nil
}
