// Command metrics-get fetches a Prometheus /metrics page, for the staging
// containers that have neither curl nor wget.
//
//	metrics-get [-url http://127.0.0.1:9818/metrics] [-n 1] [-q]
//
// The body goes to stdout (once, from the last fetch). A summary line with the
// HTTP status, the body size and the time of each fetch goes to stderr:
//
//	metrics-get: 200 OK, 812345 bytes, avg_ms=38.2 min_ms=35.1 max_ms=41.0 fetches=5
//
// It exits with 1 if a fetch fails or the status is not 200.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:9818/metrics", "URL to fetch")
	n := flag.Int("n", 1, "number of fetches; the timing summary covers all of them")
	quiet := flag.Bool("q", false, "do not print the body")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout per fetch")
	flag.Parse()
	if *n < 1 {
		fmt.Fprintln(os.Stderr, "metrics-get: -n must be at least 1")
		os.Exit(2)
	}

	hc := &http.Client{Timeout: *timeout}
	var body []byte
	var status string
	var total, lo, hi time.Duration
	for i := 0; i < *n; i++ {
		start := time.Now()
		resp, err := hc.Get(*url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metrics-get: %v\n", err)
			os.Exit(1)
		}
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		d := time.Since(start)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metrics-get: read body: %v\n", err)
			os.Exit(1)
		}
		status = resp.Status
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "metrics-get: %s\n", status)
			os.Exit(1)
		}
		total += d
		if i == 0 || d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if !*quiet {
		os.Stdout.Write(body)
	}
	avg := total / time.Duration(*n)
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }
	fmt.Fprintf(os.Stderr, "metrics-get: %s, %d bytes, avg_ms=%s min_ms=%s max_ms=%s fetches=%d\n",
		status, len(body), ms(avg), ms(lo), ms(hi), *n)
}
