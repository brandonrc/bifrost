// Command gateway-load is the standalone load-rig tool for Wave 1 Task
// 16's p99 evidence requirement (SPEC.md's REQUIREMENTS-amendment: the
// gateway path needs latency evidence, not just a green contract replay
// — see docs/adr/0005-gateway-p99-evidence.md for the recorded numbers
// and scripts/gateway-load.sh for the harness that drives this tool).
//
// It is intentionally NOT part of the shipped bifrost binary (it lives
// outside cmd/bifrost) and adds no third-party dependency — stdlib only,
// so building/running it never touches go.mod.
//
// Two subcommands:
//
//	gateway-load fake --bind ADDR
//	    Runs a trivial concurrent HTTP server that answers every request
//	    immediately with a small fixed JSON body — a stand-in for a Ray
//	    dashboard/job-API endpoint that is cheap enough not to dominate
//	    the measurement. Real Ray isn't available in this environment
//	    (no cluster to provision against); this isolates exactly the
//	    question Task 16 asks: how much latency does the Go gateway's
//	    own proxy path (auth/host-match/body-buffer/southbound-request)
//	    add, independent of any upstream's own behavior.
//
//	gateway-load bench --dial ADDR --host HOST --path PATH -n N -c C
//	    Fires N HTTP GET requests (C concurrent workers) at PATH,
//	    connecting to ADDR but sending "Host: HOST" — so the same tool
//	    drives either the fake upstream directly (baseline) or the
//	    bifrost gateway's host-routed proxy path (through-gateway),
//	    without needing /etc/hosts or real DNS. Prints p50/p90/p99/max/
//	    mean latency, error count, and throughput as JSON to stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gateway-load <fake|bench> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "fake":
		runFake(os.Args[2:])
	case "bench":
		runBench(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want fake|bench)\n", os.Args[1])
		os.Exit(2)
	}
}

func runFake(args []string) {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	bind := fs.String("bind", "127.0.0.1:0", "address to bind the fake upstream on")
	_ = fs.Parse(args)

	mux := http.NewServeMux()
	body := []byte(`{"ok":true,"job_id":"fake","status":"SUCCEEDED"}`)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	// Print the bound address (useful when --bind used port 0) so the
	// driving shell script can capture it.
	fmt.Println(ln.Addr().String())
	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

type result struct {
	N           int     `json:"n"`
	Concurrency int     `json:"concurrency"`
	Errors      int64   `json:"errors"`
	DurationS   float64 `json:"duration_s"`
	RPS         float64 `json:"rps"`
	MeanMs      float64 `json:"mean_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P90Ms       float64 `json:"p90_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
}

func runBench(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dial := fs.String("dial", "127.0.0.1:8484", "actual TCP address to connect to (host:port)")
	host := fs.String("host", "", "Host header to send (routing hostname); defaults to --dial")
	path := fs.String("path", "/", "request path")
	n := fs.Int("n", 2000, "total number of requests")
	c := fs.Int("c", 32, "concurrent workers")
	warmup := fs.Int("warmup", 50, "warmup requests (unmeasured) before the timed run")
	label := fs.String("label", "", "label to attach to the JSON result")
	_ = fs.Parse(args)

	if *host == "" {
		*host = *dial
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, *dial)
		},
		MaxIdleConns:        *c + 8,
		MaxIdleConnsPerHost: *c + 8,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	doReq := func() (time.Duration, error) {
		req, err := http.NewRequest(http.MethodGet, "http://"+*host+*path, nil)
		if err != nil {
			return 0, err
		}
		req.Host = *host
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return time.Since(start), err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		elapsed := time.Since(start)
		if resp.StatusCode >= 400 {
			return elapsed, fmt.Errorf("status %d", resp.StatusCode)
		}
		return elapsed, nil
	}

	// Warmup: establish keep-alive connections so the timed run doesn't
	// pay TCP/TLS-handshake cost inside the measured latencies.
	for i := 0; i < *warmup; i++ {
		_, _ = doReq()
	}

	latencies := make([]time.Duration, *n)
	var errCount int64
	var idx int64

	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&idx, 1) - 1
				if i >= int64(*n) {
					return
				}
				d, err := doReq()
				latencies[i] = d
				if err != nil {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}()
	}
	wg.Wait()
	total := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(p * float64(len(latencies)))
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return float64(latencies[idx]) / float64(time.Millisecond)
	}
	var sum time.Duration
	for _, d := range latencies {
		sum += d
	}
	mean := float64(sum) / float64(len(latencies)) / float64(time.Millisecond)

	res := result{
		N:           *n,
		Concurrency: *c,
		Errors:      errCount,
		DurationS:   total.Seconds(),
		RPS:         float64(*n) / total.Seconds(),
		MeanMs:      mean,
		P50Ms:       pct(0.50),
		P90Ms:       pct(0.90),
		P99Ms:       pct(0.99),
		MaxMs:       float64(latencies[len(latencies)-1]) / float64(time.Millisecond),
	}
	out := struct {
		Label string `json:"label,omitempty"`
		result
	}{Label: *label, result: res}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
