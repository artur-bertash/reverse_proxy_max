// Capacity runs the $12 Server Challenge's capacity search
//
//	go run cmd/capacity/main.go -entry v0      (from the repo root)
//
// Results go to results/<entry>/ (replaced on every run):
//
//	mload.txt     every probe's mload output, each after a "== ... ==" header with its start time
//	prometheus/   a Prometheus snapshot of the whole run (Prometheus is wiped before the run)
//
// mload's numbers are in the snapshot too: mload runs through mload-to-prom on the load generator,
// which writes them for node_exporter's textfile collector every 5 s. A run marker (capacity.prom)
// gives the results viewer the run's entry, start, end and score.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	startUsers = 2500
	minUsers   = 250
	maxUsers   = 60000 // one load generator can open only ~60k connections to one IP and port
	probeHold  = 2 * time.Minute
	finalHold  = 5 * time.Minute
	precision  = 0.05 // stop narrowing down when the range is within 5%
	stepUsers  = 250  // user counts are rounded down to a multiple of this

	maxLoadgenCPU = 70.0 // above this, the load generator may be the limit
)

// all set by flags from ssg config
var proxyServer, backendServer, loadgenServer string
var proxyAddress string

// What is being tested (-entry), and where the results go: results/<entry>.
var entry, outputDir string

// When the run started recording (after Prometheus was wiped).
var runStart time.Time

// recording is true once Prometheus has been wiped for this run; from then on,
// stopping early still saves a snapshot.
var recording bool

// The monitoring folder on the load generator (Docker Compose project "monitoring"),
// and node_exporter's textfile folder there (mload.prom, capacity.prom).
const (
	monitoringDir = "/opt/rp/monitoring"
	textfileDir   = "/var/lib/node_exporter/textfile"
)

type Verdict string

const (
	Pass    Verdict = "pass"
	Fail    Verdict = "fail"
	Invalid Verdict = "invalid" // the load generator was the limit, not the proxy
)

// MloadResult is the part of mload's JSON result that we use.
type MloadResult struct {
	RequestsPerSecond float64          `json:"req_per_s"`
	P95ms             float64          `json:"p95_ms"`
	P99ms             float64          `json:"p99_ms"`
	ErrorPercent      float64          `json:"err_pct"`
	Pass              bool             `json:"pass"`
	Errors            map[string]int64 `json:"errors"`
}

// Probe is one load test at one user count.
type Probe struct {
	Users   int
	Hold    time.Duration
	Verdict Verdict
	Load    MloadResult

	ProxyCPU      float64 // average % busy on the proxy server
	ProxySteal    float64 // average % of CPU time taken by other VMs on the same host
	LoadgenCPU    float64 // average % busy on the load generator
	ProxyMemoryMB int     // highest memory use of the proxy process
	MaxSockets    int     // highest number of open TCP sockets on the proxy server (≈ connections)
	MaxTimeWait   int     // highest number of TCP sockets in TIME_WAIT on the proxy server
	ProxyState    string  // "ok", or why the proxy stopped, e.g. "oom-kill" (from systemd)
}

func main() {
	flag.StringVar(&proxyServer, "proxy", "rp-proxy", "proxy server (SSH host name)")
	flag.StringVar(&backendServer, "backend", "rp-backend", "backend server (SSH host name)")
	flag.StringVar(&loadgenServer, "loadgen", "rp-loadgen", "load generator (SSH host name)")
	flag.StringVar(&entry, "entry", "", "what is being tested, e.g. v0, v1, nginx-stock (required)")
	flag.Parse()
	if entry == "" || strings.ContainsAny(entry, "/ \"'") {
		log.Fatal("-entry is required, e.g. -entry v0")
	}
	log.SetFlags(log.Ltime)
	outputDir = filepath.Join("results", entry)
	if err := os.RemoveAll(outputDir); err != nil { // a new run of an entry replaces the old one
		log.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Fatal(err)
	}
	proxyAddress = addressOf(proxyServer)

	prepareServers()

	warmUp()
	lastPass, firstFail := findRangeByDoubling()
	if firstFail > 0 {
		lastPass = narrowDown(lastPass, firstFail)
	}
	score := confirmWithHold(lastPass)

	restoreServers()
	appendFile("mload.txt", fmt.Sprintf("== SCORE %d users ==\n", score))
	markRunEnd(score)
	saveSnapshot()
	log.Printf("SCORE: %d users (5-minute hold passed). Results in %s", score, outputDir)
}

// fatalf stops the test. Once the run is recording, it first notes why in mload.txt
// and saves the Prometheus snapshot, so a run that stopped early can still be looked at.
func fatalf(format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	log.Print(reason)
	if recording {
		recording = false // saveSnapshot may call fatalf itself
		appendFile("mload.txt", fmt.Sprintf("== ABORTED: %s ==\n", reason))
		restoreServers()
		markRunEnd(0)
		saveSnapshot()
	}
	os.Exit(1)
}

// ---- the search ----

func warmUp() {
	log.Print("warm-up (not scored)")
	runProbe(1000, probeHold, "warmup")
}

// findRangeByDoubling returns the last user count that passed and the first that failed.
// firstFail is 0 if the proxy passed all the way up to maxUsers.
func findRangeByDoubling() (lastPass, firstFail int) {
	users := startUsers
	for {
		probe := runProbe(users, probeHold, "probe")

		switch probe.Verdict {
		case Invalid:
			stopBecauseLoadSideIsTheLimit(users)

		case Pass:
			lastPass = users
			if firstFail > 0 { // we were halving down and found a pass
				return lastPass, firstFail
			}
			if users >= maxUsers {
				log.Printf("passed %d users: that's as far as one load generator can go", users)
				return lastPass, 0
			}
			users = min(users*2, maxUsers)

		case Fail:
			firstFail = users
			if lastPass > 0 {
				return lastPass, firstFail
			}
			users /= 2 // even the first probe failed, so go down instead
			if users < minUsers {
				fatalf("failed even at %d users", minUsers)
			}
		}
	}
}

// narrowDown bisects between a passing and a failing user count until they're within 5%.
func narrowDown(lastPass, firstFail int) int {
	for float64(firstFail-lastPass)/float64(firstFail) > precision {
		users := roundDown((lastPass+firstFail)/2, stepUsers)
		if users <= lastPass {
			break
		}

		probe := runProbe(users, probeHold, "probe")
		switch probe.Verdict {
		case Invalid:
			stopBecauseLoadSideIsTheLimit(users)
		case Pass:
			lastPass = users
		case Fail:
			firstFail = users
		}
	}
	return lastPass
}

// confirmWithHold runs 5-minute holds, stepping down 5% each time one fails.
func confirmWithHold(users int) int {
	for users >= minUsers {
		probe := runProbe(users, finalHold, "hold")
		switch probe.Verdict {
		case Invalid:
			stopBecauseLoadSideIsTheLimit(users)
		case Pass:
			return users
		}
		users = roundDown(users*95/100, stepUsers)
	}
	fatalf("no 5-minute hold passed")
	return 0
}

func stopBecauseLoadSideIsTheLimit(users int) {
	fatalf("the load generator was the limit at %d users, so the result doesn't count. "+
		"Fix the load side (more IPs or load generators) and run again.", users)
}

// ---- one probe ----

func runProbe(users int, hold time.Duration, label string) Probe {
	ramp := rampFor(users)
	log.Printf("probe: %d users (ramp %s, hold %s)", users, ramp, hold)

	resetBackend()
	restartProxy()

	start := time.Now()
	appendFile("mload.txt", fmt.Sprintf("== %s %d users, start %s, ramp %s, hold %s ==\n",
		label, users, start.UTC().Format(time.RFC3339), ramp, hold))
	load := runMload(users, ramp, hold, label)
	end := time.Now()
	time.Sleep(11 * time.Second) // Prometheus collects every 5 s: wait for two more rounds

	probe := Probe{Users: users, Hold: hold, Load: load}
	readServerNumbers(&probe, start, end)
	probe.ProxyState = proxyState()
	probe.Verdict = decideVerdict(probe)
	appendFile("mload.txt", fmt.Sprintf("== %s, proxy %s, end %s ==\n\n",
		probe.Verdict, probe.ProxyState, end.UTC().Format(time.RFC3339)))
	markProbeResult(probe, label, start)

	logProbe(probe)
	return probe
}

// rampFor gives users at least 60 s to arrive, and at most ~3,000 new users per second.
func rampFor(users int) time.Duration {
	return max(60*time.Second, time.Duration(users/3000)*time.Second)
}

func decideVerdict(probe Probe) Verdict {
	if probe.LoadgenCPU > maxLoadgenCPU {
		return Invalid
	}
	for kind := range probe.Load.Errors {
		if strings.Contains(kind, "cannot assign requested address") { // load generator out of source ports
			return Invalid
		}
	}
	if probe.Load.Pass && probe.ProxyState == "ok" {
		return Pass
	}
	return Fail
}

// ---- talking to the servers ----

// onServer runs a shell command on a server and returns what it printed.
// Only the command's normal output is returned; ssh's own messages and the command's errors
// go into the error, so they can never end up in the numbers we read.
func onServer(server, command string) (string, error) {
	cmd := exec.Command("ssh", "-T", "-o", "BatchMode=yes", server, command)
	var output, errorOutput bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &errorOutput

	if err := cmd.Run(); err != nil {
		return output.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(errorOutput.String()))
	}
	return output.String(), nil
}

// mustOnServer is onServer, but stops the whole test if the command fails.
func mustOnServer(server, command string) string {
	output, err := onServer(server, command)
	if err != nil {
		fatalf("on %s: %q failed: %v\n%s", server, command, err, output)
	}
	return output
}

// addressOf looks up the IP address behind an SSH host name in ~/.ssh/config.
func addressOf(server string) string {
	config, err := exec.Command("ssh", "-G", server).Output() // prints the settings ssh would use
	if err != nil {
		log.Fatalf("could not look up %s in ~/.ssh/config: %v", server, err)
	}
	for _, line := range strings.Split(string(config), "\n") {
		if address, found := strings.CutPrefix(line, "hostname "); found {
			return address
		}
	}
	log.Fatalf("no address for %s in ~/.ssh/config", server)
	return ""
}

func prepareServers() {
	restartProxy() // it may still be down from an earlier crash
	wipePrometheus()
	checkPrometheus()
	runStart = time.Now()
	writeRunMarker(fmt.Sprintf("capacity_run_start_ms{entry=%q} %d\n", entry, runStart.UnixMilli()))

	// Only the proxy should run on the proxy server, so pause Ubuntu's background package services.
	log.Print("pausing background package services on the proxy server")
	onServer(proxyServer, "systemctl stop unattended-upgrades packagekit apt-daily.timer apt-daily-upgrade.timer")
}

func restoreServers() {
	onServer(proxyServer, "systemctl start unattended-upgrades apt-daily.timer apt-daily-upgrade.timer")
}

func resetBackend() {
	mustOnServer(backendServer, "backend-reset") // fresh copy of the database, waits for /health
}

func restartProxy() {
	mustOnServer(proxyServer, "systemctl restart proxy")

	// Wait until the proxy answers (any HTTP answer means it's listening).
	for attempt := 0; attempt < 100; attempt++ {
		response, err := http.Get("http://" + proxyAddress + "/health")
		if err == nil {
			response.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fatalf("the proxy did not start")
}

// runMload runs the load test, adds mload's output to mload.txt, and returns its result.
// mload runs through mload-to-prom, so Prometheus gets its numbers every 5 s (in the snapshot too).
func runMload(users int, ramp, hold time.Duration, label string) MloadResult {
	command := fmt.Sprintf(
		"rm -f /tmp/rp-mload.json %s/mload.prom; ulimit -n 1048576; mload -target %s -ports 80 -users %d -ramp %s -hold %s "+
			"-think-scale 1 -tokens /opt/rp/tokens.json -out /tmp/rp-mload.json | mload-to-prom %s %s %d",
		textfileDir, proxyAddress, users, ramp, hold, entry, label, users)
	output, _ := onServer(loadgenServer, command)
	appendFile("mload.txt", output)

	var result MloadResult
	resultJSON, err := onServer(loadgenServer, "cat /tmp/rp-mload.json")
	if err != nil || json.Unmarshal([]byte(resultJSON), &result) != nil {
		log.Printf("could not read mload's result; see mload.txt")
	}
	return result
}

// ---- Prometheus (on the load generator): wipe before, snapshot after ----

// wipePrometheus starts Prometheus on an empty database, so the snapshot at the end holds only this run.
func wipePrometheus() {
	log.Print("wiping Prometheus on the load generator")
	mustOnServer(loadgenServer, "cd "+monitoringDir+" && docker compose rm -sf prometheus && "+
		"docker volume rm monitoring_prometheus-data && docker compose up -d prometheus")
	recording = true
}

// writeTextfile (re)writes a file in node_exporter's textfile folder on the load generator,
// whole (via a temporary file), so Prometheus collects the metrics in it.
func writeTextfile(name, metrics string) {
	path := textfileDir + "/" + name
	mustOnServer(loadgenServer, fmt.Sprintf("printf %%s %s > %s.tmp && mv %s.tmp %s", shellQuote(metrics), path, path, path))
}

// writeRunMarker (re)writes capacity.prom: the run's entry, start, end and score as metrics,
// which the dashboard's Runs table reads from the snapshots.
func writeRunMarker(metrics string) { writeTextfile("capacity.prom", metrics) }

// markProbeResult tells the dashboard how a probe ended. A probe's verdict is only known after it,
// so the metric's value is the probe's start time: the dashboard puts a red (fail/invalid) or
// green (pass) marker there ("use series value as timestamp"). It's collected once, then removed.
func markProbeResult(p Probe, label string, start time.Time) {
	writeTextfile("probe.prom", fmt.Sprintf(
		"capacity_probe_result{entry=%q,label=%q,users=\"%d\",verdict=%q,proxy=%q,p95_ms=\"%.0f\",err_pct=\"%.2f\"} %d\n",
		entry, label, p.Users, p.Verdict, p.ProxyState, p.Load.P95ms, p.Load.ErrorPercent, start.UnixMilli()))
	time.Sleep(6 * time.Second) // Prometheus collects every 5 s
	onServer(loadgenServer, "rm -f "+textfileDir+"/probe.prom")
}

// markRunEnd adds the end time and score (0 if the run stopped early) to the run marker,
// and waits until Prometheus has collected it.
func markRunEnd(score int) {
	if runStart.IsZero() { // stopped before the run started recording
		return
	}
	writeRunMarker(fmt.Sprintf("capacity_run_start_ms{entry=%q} %d\ncapacity_run_end_ms{entry=%q} %d\ncapacity_score_users{entry=%q} %d\n",
		entry, runStart.UnixMilli(), entry, time.Now().UnixMilli(), entry, score))
	time.Sleep(11 * time.Second) // Prometheus collects every 5 s: wait for two rounds
}

// saveSnapshot snapshots Prometheus and unpacks it into results/<entry>/prometheus.
func saveSnapshot() {
	log.Print("saving the Prometheus snapshot")
	output, err := onServer(loadgenServer, "curl -sf -XPOST 127.0.0.1:9090/api/v1/admin/tsdb/snapshot")
	var response struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(output), &response) != nil || response.Data.Name == "" {
		fatalf("could not take the Prometheus snapshot: %v %s", err, output)
	}

	dir := filepath.Join(outputDir, "prometheus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("%v", err)
	}
	remote := fmt.Sprintf("cd %s && docker compose exec -T prometheus tar -C /prometheus/snapshots/%s -czf - .",
		monitoringDir, response.Data.Name)
	copyCmd := exec.Command("sh", "-c", fmt.Sprintf("ssh -T -o BatchMode=yes %s %s | tar -xzf - -C %s",
		loadgenServer, shellQuote(remote), shellQuote(dir)))
	if output, err := copyCmd.CombinedOutput(); err != nil {
		fatalf("could not copy the snapshot %s: %v %s", response.Data.Name, err, output)
	}

	blocks, _ := filepath.Glob(filepath.Join(dir, "*", "meta.json"))
	if len(blocks) == 0 {
		fatalf("the snapshot in %s has no data blocks", dir)
	}
	log.Printf("snapshot saved: %d blocks in %s", len(blocks), dir)
	onServer(loadgenServer, "rm -f "+textfileDir+"/capacity.prom") // the run is over
}

// ---- numbers from Prometheus (on the load generator) ----

// The numbers for each probe, as Prometheus queries over a probe's time window ($W).
// The servers are told apart by the "server" label set in prometheus.yml.
var serverQueries = map[string]string{
	"proxy_cpu":   `100 * (1 - avg(rate(node_cpu_seconds_total{server="proxy",mode="idle"}[$W])))`,
	"proxy_steal": `100 * avg(rate(node_cpu_seconds_total{server="proxy",mode="steal"}[$W]))`,
	"loadgen_cpu": `100 * (1 - avg(rate(node_cpu_seconds_total{server="loadgen",mode="idle"}[$W])))`,
	"proxy_rss":   `max_over_time(process_resident_memory_bytes{job="proxy"}[$W])`,
	"sockets":     `max_over_time(node_sockstat_TCP_alloc{server="proxy"}[$W])`,
	"time_wait":   `max_over_time(node_sockstat_TCP_tw{server="proxy"}[$W])`,
}

// checkPrometheus waits (up to a minute) until Prometheus collects from every server,
// and stops the test if it doesn't: without it, the probes would have no CPU or memory numbers.
func checkPrometheus() {
	log.Print("waiting until Prometheus collects from all servers")
	targets := []string{"node/proxy", "node/backend", "node-loadgen/loadgen", "proxy/proxy"}
	var missing string
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(2 * time.Second)
		output, err := onServer(loadgenServer, "curl -sfG 127.0.0.1:9090/api/v1/query --data-urlencode query=up")
		var response prometheusResponse
		if err != nil || json.Unmarshal([]byte(output), &response) != nil {
			missing = "Prometheus itself (not answering)"
			continue
		}
		up := map[string]bool{}
		for _, series := range response.Data.Result {
			up[series.Metric["job"]+"/"+series.Metric["server"]] = series.Value[1] == "1"
		}
		missing = ""
		for _, target := range targets {
			if !up[target] {
				missing = target
			}
		}
		if missing == "" {
			return
		}
	}
	fatalf("Prometheus can't collect from %s; see http://127.0.0.1:9090/targets on %s", missing, loadgenServer)
}

type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// queryPrometheus runs one query at one moment and returns the single number it gives.
// No data (e.g. the proxy never started) gives 0 and a warning; Prometheus not answering stops the test.
func queryPrometheus(query string, at time.Time) float64 {
	command := fmt.Sprintf("curl -sfG 127.0.0.1:9090/api/v1/query --data-urlencode %s --data-urlencode time=%d",
		shellQuote("query="+query), at.Unix())
	output := mustOnServer(loadgenServer, command)

	var response prometheusResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil || response.Status != "success" {
		fatalf("Prometheus query failed: %s\n%s", query, output)
	}
	if len(response.Data.Result) == 0 {
		log.Printf("no data in Prometheus for: %s", query)
		return 0
	}
	text, _ := response.Data.Result[0].Value[1].(string)
	value, _ := strconv.ParseFloat(text, 64)
	return value
}

// readServerNumbers fills in a probe's CPU, memory and socket numbers for its time window.
func readServerNumbers(probe *Probe, start, end time.Time) {
	window := fmt.Sprintf("%ds", int(end.Sub(start).Seconds()))
	number := func(name string) float64 {
		return queryPrometheus(strings.ReplaceAll(serverQueries[name], "$W", window), end)
	}
	probe.ProxyCPU = number("proxy_cpu")
	probe.ProxySteal = number("proxy_steal")
	probe.LoadgenCPU = number("loadgen_cpu")
	probe.ProxyMemoryMB = int(number("proxy_rss") / (1 << 20))
	probe.MaxSockets = int(number("sockets"))
	probe.MaxTimeWait = int(number("time_wait"))
}

// proxyState asks systemd whether the proxy is still running, and if not, why it stopped.
// After an out-of-memory kill, systemd says Result=oom-kill.
func proxyState() string {
	output := mustOnServer(proxyServer, "systemctl show proxy -p ActiveState -p Result")
	state := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			state[key] = value
		}
	}
	if state["ActiveState"] == "active" {
		return "ok"
	}
	if state["Result"] != "" && state["Result"] != "success" {
		return state["Result"]
	}
	return "stopped"
}

// shellQuote wraps text in single quotes for the remote shell.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// ---- results ----

func logProbe(p Probe) {
	log.Printf("  → %s (proxy %s): %.0f req/s, p95 %.1f ms, p99 %.1f ms, errors %.2f%%, proxy CPU %.0f%%, "+
		"proxy RAM %d MB, sockets %d, TIME_WAIT %d, load generator CPU %.0f%% %s",
		p.Verdict, p.ProxyState, p.Load.RequestsPerSecond, p.Load.P95ms, p.Load.P99ms, p.Load.ErrorPercent,
		p.ProxyCPU, p.ProxyMemoryMB, p.MaxSockets, p.MaxTimeWait, p.LoadgenCPU, errorKinds(p))
}

// shortErrorNames keeps the log line short.
var shortErrorNames = map[string]string{
	"connection refused":       "refused",
	"connection reset by peer": "reset",
}

// errorKinds turns mload's error counts into one short line, largest first,
// e.g. "refused 290422, timeout 24064".
func errorKinds(p Probe) string {
	type kindCount struct {
		kind  string
		count int64
	}
	var counts []kindCount
	for kind, count := range p.Load.Errors {
		if short, ok := shortErrorNames[kind]; ok {
			kind = short
		}
		counts = append(counts, kindCount{kind, count})
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].count > counts[j].count })

	var kinds []string
	for _, c := range counts {
		kinds = append(kinds, fmt.Sprintf("%s %d", c.kind, c.count))
	}
	return strings.Join(kinds, ", ")
}

func appendFile(name, content string) {
	file, err := os.OpenFile(filepath.Join(outputDir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	file.WriteString(content)
}

func roundDown(n, step int) int {
	return n / step * step
}
