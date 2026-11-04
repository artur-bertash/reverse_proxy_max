package main

import (
	"flag"
	"log"
	"net/http"
)

func myHandler(w http.ResponseWriter, r *http.Request) {

	log.Printf("%v\t%v\t%v\tHost: %v", r.RemoteAddr, r.Method, r.URL, r.Host)
	for name, headers := range r.Header {
		for _, h := range headers {
			log.Printf("%v: %v", name, h)
		}
	}

}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()

	log.Println("Starting server on", *addr)
	if err := http.ListenAndServe(*addr, http.HandlerFunc(myHandler)); err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}
