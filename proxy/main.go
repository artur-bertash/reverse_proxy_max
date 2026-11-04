package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

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

func loadBalancingReverseProxy(target1, target2 *url.URL) *httputil.ReverseProxy {
	var targetNum = 1

	director := func(req *http.Request) {
		var target *url.URL
		if targetNum == 1 {
			target = target1
			targetNum = 2
			fmt.Println("This requst is forwarder to FIRST ONE")
		} else {
			target = target2
			targetNum = 1
			fmt.Println("This requst is forwarder to SECOND ONE")
		}

		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
	}
	return &httputil.ReverseProxy{Director: director}

}
func main() {

	fromAddr := flag.String("from", "127.0.0.1:9090", "proxy's listening address")
	toAddrOne := flag.String("to1", "127.0.0.1:8080", "the address this proxy will forward to")
	toAddrTwo := flag.String("to2", "127.0.0.1:8080", "the address this proxy will forward to")
	flag.Parse()

	toUrlOne := parseToUrl(*toAddrOne)
	toUrlTwo := parseToUrl(*toAddrTwo)

	proxy := loadBalancingReverseProxy(toUrlOne, toUrlTwo)
	log.Println("Starting proxy server on", *fromAddr)
	if err := http.ListenAndServe(*fromAddr, proxy); err != nil {
		log.Fatal("ListenAndServer", err)
	}
}
