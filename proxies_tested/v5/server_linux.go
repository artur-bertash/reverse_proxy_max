//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxHeaderBytes = 1 << 20
	maxBodyBytes   = 64 << 20
	writeTimeout   = 60 * time.Second
	epollET        = 1 << 31
	soReusePort    = 0xf
)

type conn struct {
	fd           int
	mu           sync.Mutex
	in           []byte
	busy         bool
	gone         bool
	closed       bool
	sentContinue bool
	writable     chan struct{}
}

type server struct {
	proxy *proxy
	conns []atomic.Pointer[conn]
	epfd  int
	buf   []byte
}

func serve(address string, p *proxy) error {
	lfd, err := listen(address)
	if err != nil {
		return err
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return err
	}
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, lfd,
		&syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(lfd)}); err != nil {
		return err
	}
	s := &server{proxy: p, conns: make([]atomic.Pointer[conn], limit.Cur), epfd: epfd, buf: make([]byte, 64<<10)}

	events := make([]syscall.EpollEvent, 512)
	for {
		n, err := syscall.EpollWait(epfd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == lfd {
				s.accept(lfd)
				continue
			}
			c := s.conns[fd].Load()
			if c == nil {
				continue
			}

			ev := events[i].Events
			if ev&(syscall.EPOLLOUT|syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				select {
				case c.writable <- struct{}{}:
				default:
				}
			}
			if ev&(syscall.EPOLLIN|syscall.EPOLLRDHUP|syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				s.read(c, ev&(syscall.EPOLLRDHUP|syscall.EPOLLHUP|syscall.EPOLLERR) != 0)
			}
		}
	}
}

func listen(address string) (int, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return -1, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return -1, err
	}
	ip := net.ParseIP(host)
	wildcard := host == "" || ip.IsUnspecified()

	family := syscall.AF_INET
	if wildcard || ip.To4() == nil {
		family = syscall.AF_INET6
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil && wildcard {
		family = syscall.AF_INET
		fd, err = syscall.Socket(family, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	}
	if err != nil {
		return -1, err
	}
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1); err != nil {
		return -1, err
	}
	var sa syscall.Sockaddr
	switch {
	case family == syscall.AF_INET:
		a := &syscall.SockaddrInet4{Port: port}
		if !wildcard {
			copy(a.Addr[:], ip.To4())
		}
		sa = a
	default:
		a := &syscall.SockaddrInet6{Port: port}
		if wildcard {
			syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0)
		} else {
			copy(a.Addr[:], ip.To16())
		}
		sa = a
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return -1, err
	}
	if err := syscall.Listen(fd, 65535); err != nil {
		return -1, err
	}
	return fd, nil
}

func (s *server) accept(lfd int) {
	for {
		r, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(lfd), 0, 0,
			syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0, 0)
		if errno != 0 {
			switch errno {
			case syscall.EAGAIN:
				return
			case syscall.EINTR, syscall.ECONNABORTED:
				continue
			case syscall.EMFILE, syscall.ENFILE:
				time.Sleep(5 * time.Millisecond)
				return
			}
			log.Printf("accept: %v", errno)
			return
		}
		fd := int(r)
		if fd >= len(s.conns) {
			syscall.Close(fd)
			continue
		}
		syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		c := &conn{fd: fd, writable: make(chan struct{}, 1)}
		s.conns[fd].Store(c)
		if err := syscall.EpollCtl(s.epfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{
			Events: syscall.EPOLLIN | syscall.EPOLLOUT | syscall.EPOLLRDHUP | epollET, Fd: int32(fd)}); err != nil {
			s.conns[fd].Store(nil)
			syscall.Close(fd)
		}
	}
}

func (s *server) read(c *conn, hangup bool) {
	for {
		n, err := syscall.Read(c.fd, s.buf)
		if n > 0 {
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			c.in = append(c.in, s.buf[:n]...)
			flood := len(c.in) > maxHeaderBytes+maxBodyBytes
			start := !c.busy
			c.busy = true
			c.mu.Unlock()
			if start {
				go s.work(c)
			}
			if flood {
				s.leave(c)
				return
			}
			if n < len(s.buf) && !hangup {
				return
			}
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN {
			return
		}
		s.leave(c)
		return
	}
}

func (s *server) leave(c *conn) {
	c.mu.Lock()
	c.gone = true
	if !c.busy {
		s.closeLocked(c)
	}
	c.mu.Unlock()
	select {
	case c.writable <- struct{}{}:
	default:
	}
}

func (s *server) closeLocked(c *conn) {
	if c.closed {
		return
	}
	c.closed = true
	c.in = nil
	s.conns[c.fd].CompareAndSwap(c, nil)
	syscall.Close(c.fd)
}

func (s *server) work(c *conn) {
	for {
		c.mu.Lock()
		if c.closed {
			c.busy = false
			c.mu.Unlock()
			return
		}
		raw, expectContinue, err := next(c)
		if raw == nil {
			if err != nil {

				c.in = nil
				c.mu.Unlock()
				s.writeAll(c, errorResponse(err))
				c.mu.Lock()
				s.closeLocked(c)
				c.busy = false
				c.mu.Unlock()
				return
			}
			if expectContinue && !c.sentContinue && !c.gone {
				c.sentContinue = true
				syscall.Write(c.fd, []byte("HTTP/1.1 100 Continue\r\n\r\n"))
			}
			if c.gone {
				s.closeLocked(c)
			}
			c.busy = false
			c.mu.Unlock()
			return
		}
		c.sentContinue = false
		c.mu.Unlock()

		if !s.handle(c, raw) {
			c.mu.Lock()
			s.closeLocked(c)
			c.busy = false
			c.mu.Unlock()
			return
		}
	}
}

func next(c *conn) (raw []byte, expectContinue bool, err error) {
	if len(c.in) == 0 {
		return nil, false, nil
	}
	length, expectContinue, err := requestLength(c.in)
	if err != nil || length == 0 {
		return nil, expectContinue, err
	}
	if length == len(c.in) {
		raw, c.in = c.in, nil
		return raw, false, nil
	}
	raw = make([]byte, length)
	copy(raw, c.in)
	c.in = append(c.in[:0], c.in[length:]...)
	return raw, false, nil
}

var (
	errTooLarge = errors.New("request too large")
	errBadFrame = errors.New("malformed request framing")
)

func errorResponse(err error) []byte {
	if err == errTooLarge {
		return []byte("HTTP/1.1 413 Request Entity Too Large\r\nContent-Type: text/plain; charset=utf-8\r\n" +
			"Connection: close\r\n\r\n413 Request Entity Too Large")
	}
	return []byte("HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Connection: close\r\n\r\n400 Bad Request")
}

func requestLength(b []byte) (length int, expectContinue bool, err error) {
	end := bytes.Index(b, []byte("\r\n\r\n"))
	if end < 0 {
		if len(b) > maxHeaderBytes {
			return 0, false, errTooLarge
		}
		return 0, false, nil
	}
	headerEnd := end + 4
	contentLength, chunked := int64(0), false
	header := b[:end]
	if i := bytes.IndexByte(header, '\n'); i >= 0 {
		header = header[i+1:]
	} else {
		header = nil
	}
	for len(header) > 0 {
		line := header
		if i := bytes.IndexByte(header, '\n'); i >= 0 {
			line, header = header[:i], header[i+1:]
		} else {
			header = nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name, value := line[:colon], bytes.TrimSpace(line[colon+1:])
		switch {
		case bytes.EqualFold(name, []byte("Content-Length")):
			n, perr := strconv.ParseInt(string(value), 10, 64)
			if perr != nil || n < 0 {
				return 0, false, errBadFrame
			}
			contentLength = n
		case bytes.EqualFold(name, []byte("Transfer-Encoding")):
			chunked = bytes.Contains(bytes.ToLower(value), []byte("chunked"))
		case bytes.EqualFold(name, []byte("Expect")):
			expectContinue = bytes.EqualFold(value, []byte("100-continue"))
		}
	}
	if chunked {
		n, err := chunkedLength(b[headerEnd:])
		if err != nil || n < 0 {
			return 0, expectContinue, err
		}
		return headerEnd + n, false, nil
	}
	if contentLength > maxBodyBytes {
		return 0, false, errTooLarge
	}
	if int64(len(b)) < int64(headerEnd)+contentLength {
		return 0, expectContinue, nil
	}
	return headerEnd + int(contentLength), false, nil
}

func chunkedLength(b []byte) (int, error) {
	pos := 0
	for {
		i := bytes.IndexByte(b[pos:], '\n')
		if i < 0 {
			return -1, nil
		}
		line := bytes.TrimRight(b[pos:pos+i], "\r")
		if j := bytes.IndexByte(line, ';'); j >= 0 {
			line = line[:j]
		}
		size, err := strconv.ParseInt(string(bytes.TrimSpace(line)), 16, 64)
		if err != nil || size < 0 {
			return 0, errBadFrame
		}
		pos += i + 1
		if size == 0 {
			for {
				i := bytes.IndexByte(b[pos:], '\n')
				if i < 0 {
					return -1, nil
				}
				line := bytes.TrimRight(b[pos:pos+i], "\r")
				pos += i + 1
				if len(line) == 0 {
					return pos, nil
				}
			}
		}
		if size > maxBodyBytes || int64(pos)+size > maxBodyBytes {
			return 0, errTooLarge
		}
		if int64(len(b)) < int64(pos)+size+2 {
			return -1, nil
		}
		pos += int(size) + 2
	}
}

func (s *server) handle(c *conn, raw []byte) bool {

	size := min(max(len(raw), 16), 4096)
	r, err := http.ReadRequest(bufio.NewReaderSize(bytes.NewReader(raw), size))
	if err != nil {
		s.writeAll(c, errorResponse(errBadFrame))
		return false
	}
	clientClose := r.Close
	http10 := r.ProtoMajor == 1 && r.ProtoMinor == 0
	s.proxy.toBackend(r)

	resp, err := s.proxy.transport.RoundTrip(r)
	if err != nil {
		head := "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n"
		if clientClose {
			head += "Connection: close\r\n"
		}
		return s.writeAll(c, []byte(head+"\r\n")) == nil && !clientClose
	}
	defer resp.Body.Close()
	return s.writeResponse(c, r.Method, resp, clientClose, http10)
}

var outBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 32<<10); return &b }}

func (s *server) writeResponse(c *conn, method string, resp *http.Response, clientClose, http10 bool) bool {
	bp := outBuffers.Get().(*[]byte)
	out := (*bp)[:0]
	defer func() {
		if cap(out) <= 64<<10 {
			*bp = out[:0]
			outBuffers.Put(bp)
		}
	}()

	code := resp.StatusCode
	noBody := method == "HEAD" || code/100 == 1 || code == 204 || code == 304
	chunked, closeAfter := false, clientClose
	if !noBody && resp.ContentLength < 0 {
		if http10 {
			closeAfter = true
		} else {
			chunked = true
		}
	}

	status := resp.Status
	if status == "" {
		status = strconv.Itoa(code) + " " + http.StatusText(code)
	}
	if http10 {
		out = append(out, "HTTP/1.0 "...)
	} else {
		out = append(out, "HTTP/1.1 "...)
	}
	out = append(out, status...)
	out = append(out, "\r\n"...)

	removeHopHeaders(resp.Header)
	var namesArray [32]string
	names := namesArray[:0]
	for name := range resp.Header {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range resp.Header[name] {
			out = append(out, name...)
			out = append(out, ": "...)
			out = append(out, v...)
			out = append(out, "\r\n"...)
		}
	}
	if !noBody && resp.ContentLength >= 0 && resp.Header["Content-Length"] == nil {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, resp.ContentLength, 10)
		out = append(out, "\r\n"...)
	}
	if chunked {
		out = append(out, "Transfer-Encoding: chunked\r\n"...)
		if len(resp.Trailer) > 0 {
			out = append(out, "Trailer: "...)
			first := true
			for name := range resp.Trailer {
				if !first {
					out = append(out, ", "...)
				}
				out, first = append(out, name...), false
			}
			out = append(out, "\r\n"...)
		}
	}
	if _, ok := resp.Header["Date"]; !ok {
		out = append(out, "Date: "...)
		out = time.Now().UTC().AppendFormat(out, http.TimeFormat)
		out = append(out, "\r\n"...)
	}
	switch {
	case http10 && !closeAfter:
		out = append(out, "Connection: keep-alive\r\n"...)
	case closeAfter && !http10:
		out = append(out, "Connection: close\r\n"...)
	}
	out = append(out, "\r\n"...)

	if noBody {
		return s.writeAll(c, out) == nil && !closeAfter
	}

	if !chunked {
		var sent int64
		for {
			if len(out) == cap(out) {
				if s.writeAll(c, out) != nil {
					return false
				}
				out = out[:0]
			}
			start := len(out)
			n, err := resp.Body.Read(out[start:cap(out)])
			out = out[:start+n]
			sent += int64(n)
			if err == io.EOF {
				break
			}
			if err != nil {
				return false
			}
		}
		if s.writeAll(c, out) != nil {
			return false
		}
		return !closeAfter && (resp.ContentLength < 0 || sent == resp.ContentLength)
	}

	chunk := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			out = strconv.AppendInt(out, int64(n), 16)
			out = append(out, "\r\n"...)
			out = append(out, chunk[:n]...)
			out = append(out, "\r\n"...)
			if len(out) >= 16<<10 {
				if s.writeAll(c, out) != nil {
					return false
				}
				out = out[:0]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
	}
	out = append(out, "0\r\n"...)
	for name, values := range resp.Trailer {
		for _, v := range values {
			out = append(out, name...)
			out = append(out, ": "...)
			out = append(out, v...)
			out = append(out, "\r\n"...)
		}
	}
	out = append(out, "\r\n"...)
	return s.writeAll(c, out) == nil && !closeAfter
}

var errWriteTimeout = errors.New("write timeout")

func (s *server) writeAll(c *conn, p []byte) error {
	for len(p) > 0 {
		n, err := syscall.Write(c.fd, p)
		if n > 0 {
			p = p[n:]
			continue
		}
		switch err {
		case syscall.EINTR:
			continue
		case syscall.EAGAIN:
			select {
			case <-c.writable:
			case <-time.After(writeTimeout):
				return errWriteTimeout
			}
			continue
		case nil:
			return io.ErrShortWrite
		}
		return err
	}
	return nil
}

var workerPIDs []int

func startWorkers(n int) {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; i < n; i++ {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), workerEnv+"=1")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			log.Fatal("start worker: ", err)
		}
		workerPIDs = append(workerPIDs, cmd.Process.Pid)
		go func() {
			err := cmd.Wait()
			log.Fatalf("worker %d exited: %v", cmd.Process.Pid, err)
		}()
	}
}
