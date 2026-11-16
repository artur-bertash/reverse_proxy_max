package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
)

// limits the total amount of memory managed by go's runtime (check capacity as it runs with a flag there)
const memoryLimit = 1300 << 20 // 1300 MiB

func parseToUrl(addr string) *url.URL {
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	toUrl, err := url.Parse(addr)
	if err != nil {
		log.Fatal(err)
	}
	return toUrl
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 2450
	t.MaxIdleConnsPerHost = 2450 // default 2: a new TCP connection on almost every request
	return t
}

var copyBuffers = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

// Hop-by-hop headers belong to one connection, not to the request (RFC 9110 §7.6.1); a proxy drops them.
// Transfer-Encoding never shows up here: net/http moves it out of the header map.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Upgrade"}

func removeHopHeaders(h http.Header) {
	for _, field := range h["Connection"] { // headers the sender named as hop-by-hop
		for _, name := range strings.Split(field, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		delete(h, name) // canonical names: no allocation
	}
}

// proxy forwards every request to one backend. Unlike httputil.ReverseProxy it reuses the incoming
// request instead of cloning it and its headers, and adds nothing (no X-Forwarded-For).
type proxy struct {
	backend   *url.URL
	transport *http.Transport
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.URL.Scheme = p.backend.Scheme
	r.URL.Host = p.backend.Host
	r.RequestURI = ""
	r.Close = false
	if r.ContentLength == 0 {
		r.Body = nil
	}
	removeHopHeaders(r.Header)

	resp, err := p.transport.RoundTrip(r)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway) // the backend didn't answer: same as ReverseProxy
		return
	}
	defer resp.Body.Close()

	removeHopHeaders(resp.Header)
	h := w.Header()
	for name, values := range resp.Header {
		h[name] = values // share the slices: no copy
	}
	if len(resp.Trailer) > 0 { // announce trailers before the header goes out, else they're dropped
		names := make([]string, 0, len(resp.Trailer))
		for name := range resp.Trailer {
			names = append(names, name)
		}
		h["Trailer"] = []string{strings.Join(names, ", ")}
	}
	w.WriteHeader(resp.StatusCode)

	buf := copyBuffers.Get().(*[]byte)
	_, err = io.CopyBuffer(writerOnly{w}, resp.Body, *buf)
	copyBuffers.Put(buf)
	if err != nil {
		return // client or backend went away mid-body: nothing more to send
	}

	for name, values := range resp.Trailer { // values arrive after the body
		h[name] = values
	}
}

// writerOnly hides the ResponseWriter's ReadFrom, so io.CopyBuffer uses our pooled buffer.
type writerOnly struct{ io.Writer }

func main() {
	fromAddr := flag.String("from", "127.0.0.1:9090", "proxy's listening address")
	toAddr := flag.String("to", "127.0.0.1:3000", "the backend this proxy forwards to")
	metricsAddr := flag.String("metrics", "", "localhost address for /metrics, e.g. 127.0.0.1:9091 (off if empty)")
	pprofAddr := flag.String("pprof", "", "localhost address for /debug/pprof, e.g. 127.0.0.1:6060 (off if empty)")
	flag.Parse()

	debug.SetMemoryLimit(memoryLimit)

	if *metricsAddr != "" {
		go serveMetrics(*metricsAddr)
	}
	if *pprofAddr != "" {
		go servePprof(*pprofAddr)
	}
	handler := &proxy{backend: parseToUrl(*toAddr), transport: newTransport()}
	log.Println("Starting proxy server on", *fromAddr)
	if err := http.ListenAndServe(*fromAddr, handler); err != nil {
		log.Fatal("ListenAndServer", err)
	}
}
