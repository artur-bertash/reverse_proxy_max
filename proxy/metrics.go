package main

// Monitoring endpoints, kept apart from the request path in main.go:
//
//   - /metrics on -metrics (e.g. 127.0.0.1:9091): Go runtime and process numbers in Prometheus's
//     text format. Prometheus on the load generator reaches it through an SSH tunnel
//     (infra/roles/tunnel), so it listens on localhost only and needs no password.
//   - /debug/pprof on -pprof (e.g. 127.0.0.1:6060): profiles, localhost only. Profiling can make
//     the proxy do expensive work, so it is never reachable from outside the server.
//
// Both are separate listeners, so they never share anything with the proxy's own port.
//
// No per-request counters: the numbers are read only when Prometheus asks (every 5 s), so the
// proxy does no extra work per request. No third-party packages: runtime/metrics has it all.

import (
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime/metrics"
	"strings"
)

// serveMetrics serves /metrics. Only pass a localhost address.
func serveMetrics(address string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) { writeMetrics(w) })
	log.Println("metrics on http://" + address + "/metrics")
	log.Printf("metrics stopped: %v", http.ListenAndServe(address, mux))
}

// servePprof serves Go's profiles. Only pass a localhost address.
func servePprof(address string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	log.Println("pprof on http://" + address + "/debug/pprof/")
	log.Printf("pprof stopped: %v", http.ListenAndServe(address, mux))
}

// writeMetrics writes every single-value Go runtime metric, plus the process's memory and
// open files, in Prometheus's text format. Names follow runtime/metrics, e.g.
// "/memory/classes/heap/objects:bytes" → go_memory_classes_heap_objects_bytes.
func writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	descriptions := metrics.All()
	samples := make([]metrics.Sample, len(descriptions))
	for i, d := range descriptions {
		samples[i].Name = d.Name
	}
	metrics.Read(samples) // unlike runtime.ReadMemStats, this doesn't stop the program

	for i, sample := range samples {
		var value float64
		switch sample.Value.Kind() {
		case metrics.KindUint64:
			value = float64(sample.Value.Uint64())
		case metrics.KindFloat64:
			value = sample.Value.Float64()
		default:
			continue // histograms: not needed here
		}
		name := prometheusName(sample.Name)
		kind := "gauge"
		if descriptions[i].Cumulative {
			kind = "counter"
		}
		fmt.Fprintf(w, "# TYPE %s %s\n%s %g\n", name, kind, name, value)
	}

	// The same numbers the old monitor read from /proc: resident memory and open files, added up
	// over this process and the workers it started (-procs), so they count as one proxy.
	var totalResident int64
	var totalFiles int
	for _, dir := range procDirs() {
		var size, resident int64
		if statm, err := os.ReadFile(dir + "/statm"); err == nil {
			fmt.Sscan(string(statm), &size, &resident)
			totalResident += resident * int64(os.Getpagesize())
		}
		if files, err := os.ReadDir(dir + "/fd"); err == nil {
			totalFiles += len(files)
		}
	}
	fmt.Fprintf(w, "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes %d\n", totalResident)
	fmt.Fprintf(w, "# TYPE process_open_fds gauge\nprocess_open_fds %d\n", totalFiles)
	fmt.Fprintf(w, "# TYPE proxy_processes gauge\nproxy_processes %d\n", len(procDirs()))
}

// procDirs: /proc/self plus /proc/<pid> of every worker process.
func procDirs() []string {
	dirs := []string{"/proc/self"}
	for _, pid := range workerPIDs {
		dirs = append(dirs, fmt.Sprintf("/proc/%d", pid))
	}
	return dirs
}

// prometheusName turns "/gc/heap/allocs:bytes" into "go_gc_heap_allocs_bytes".
func prometheusName(runtimeName string) string {
	var name strings.Builder
	name.WriteString("go")
	for _, r := range runtimeName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			name.WriteRune(r)
		default:
			name.WriteByte('_')
		}
	}
	return strings.ReplaceAll(name.String(), "__", "_")
}
