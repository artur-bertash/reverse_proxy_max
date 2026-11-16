package main

import (
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

const memoryLimit = 1300 << 20

const workerEnv = "RP_WORKER"

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

func newTransport(idle int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = idle
	t.MaxIdleConnsPerHost = idle
	return t
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Upgrade"}

func removeHopHeaders(h http.Header) {
	for _, field := range h["Connection"] {
		for _, name := range strings.Split(field, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		delete(h, name)
	}
}

type proxy struct {
	backend   *url.URL
	transport *http.Transport
}

func (p *proxy) toBackend(r *http.Request) {
	r.URL.Scheme = p.backend.Scheme
	r.URL.Host = p.backend.Host
	r.RequestURI = ""
	r.Close = false
	if r.ContentLength == 0 {
		r.Body = nil
	}
	removeHopHeaders(r.Header)
}

func main() {
	fromAddr := flag.String("from", "127.0.0.1:9090", "proxy's listening address")
	toAddr := flag.String("to", "127.0.0.1:3000", "the backend this proxy forwards to")
	metricsAddr := flag.String("metrics", "", "localhost address for /metrics, e.g. 127.0.0.1:9091 (off if empty)")
	pprofAddr := flag.String("pprof", "", "localhost address for /debug/pprof, e.g. 127.0.0.1:6060 (off if empty)")
	procs := flag.Int("procs", 2, "processes sharing the port (SO_REUSEPORT); each has its own 65,535 open files")
	flag.Parse()
	if *procs < 1 {
		*procs = 1
	}

	debug.SetMemoryLimit(memoryLimit / int64(*procs))

	runtime.GOMAXPROCS(2)

	worker := os.Getenv(workerEnv) != ""
	if !worker {
		startWorkers(*procs - 1)
		if *metricsAddr != "" {
			go serveMetrics(*metricsAddr)
		}
		if *pprofAddr != "" {
			go servePprof(*pprofAddr)
		}
	}
	handler := &proxy{backend: parseToUrl(*toAddr), transport: newTransport(2450 / *procs)}
	log.Printf("Starting proxy server on %s (pid %d)", *fromAddr, os.Getpid())
	log.Fatal("serve: ", serve(*fromAddr, handler))
}
