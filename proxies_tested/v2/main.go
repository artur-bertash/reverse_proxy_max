package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strings"
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
	target := parseToUrl(*toAddr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = newTransport()
	log.Println("Starting proxy server on", *fromAddr)
	if err := http.ListenAndServe(*fromAddr, proxy); err != nil {
		log.Fatal("ListenAndServer", err)
	}
}
