// Package bench is hpcdn's built-in HTTP load generator, used by
// `hpcdn bench` to measure routing/serving throughput and latency without
// external tooling. It reports throughput, a latency histogram with
// percentiles, and error counts, over a fixed duration with a fixed
// number of concurrent virtual users.
package bench

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a run.
type Options struct {
	URL             string
	Concurrency     int
	Duration        time.Duration
	FollowRedirects bool
	Insecure        bool
	Timeout         time.Duration
}

// Result is the aggregate outcome of a run.
type Result struct {
	Requests    int64
	Errors      int64
	Non2xx3xx   int64
	BytesRead   int64
	Elapsed     time.Duration
	Throughput  float64 // requests/sec
	LatencyAvg  time.Duration
	LatencyP50  time.Duration
	LatencyP95  time.Duration
	LatencyP99  time.Duration
	LatencyMax  time.Duration
	StatusCount map[int]int64
}

// Run executes the load test until Duration elapses or ctx is cancelled.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 50
	}
	if opts.Duration <= 0 {
		opts.Duration = 15 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}

	// Bound the connection pool to exactly the worker count. The stdlib
	// defaults (MaxIdleConns=100, unlimited MaxConnsPerHost) silently churn
	// sockets when concurrency exceeds 100 — every churned socket is a
	// TIME_WAIT entry, and sustained runs exhaust the ephemeral port range.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = opts.Concurrency
	tr.MaxIdleConnsPerHost = opts.Concurrency
	tr.MaxConnsPerHost = opts.Concurrency
	if opts.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: opts.Timeout, Transport: tr}
	if !opts.FollowRedirects {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Duration)
	defer cancel()

	var (
		requests, errors, bad, bytesRead atomic.Int64
		mu                               sync.Mutex
		latencies                        []time.Duration
		statuses                         = map[int]int64{}
	)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 4096)
			localStatus := map[int]int64{}
			for runCtx.Err() == nil {
				t0 := time.Now()
				req, err := http.NewRequestWithContext(runCtx, http.MethodGet, opts.URL, nil)
				if err != nil {
					errors.Add(1)
					continue
				}
				resp, err := client.Do(req)
				if err != nil {
					if runCtx.Err() != nil {
						break
					}
					errors.Add(1)
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				bytesRead.Add(n)
				requests.Add(1)
				localStatus[resp.StatusCode]++
				if resp.StatusCode >= 400 {
					bad.Add(1)
				}
				local = append(local, time.Since(t0))
			}
			mu.Lock()
			latencies = append(latencies, local...)
			for k, v := range localStatus {
				statuses[k] += v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	res := Result{
		Requests:    requests.Load(),
		Errors:      errors.Load(),
		Non2xx3xx:   bad.Load(),
		BytesRead:   bytesRead.Load(),
		Elapsed:     elapsed,
		StatusCount: statuses,
	}
	if res.Requests > 0 {
		res.Throughput = float64(res.Requests) / elapsed.Seconds()
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		var sum time.Duration
		for _, l := range latencies {
			sum += l
		}
		res.LatencyAvg = sum / time.Duration(len(latencies))
		res.LatencyP50 = latencies[len(latencies)*50/100]
		res.LatencyP95 = latencies[min(len(latencies)*95/100, len(latencies)-1)]
		res.LatencyP99 = latencies[min(len(latencies)*99/100, len(latencies)-1)]
		res.LatencyMax = latencies[len(latencies)-1]
	}
	return res, nil
}

// Format renders a result as an aligned human-readable report.
func Format(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Requests:      %d in %s\n", r.Requests, r.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&sb, "Throughput:    %.1f req/s\n", r.Throughput)
	fmt.Fprintf(&sb, "Latency avg:   %s\n", r.LatencyAvg.Round(10*time.Microsecond))
	fmt.Fprintf(&sb, "Latency p50:   %s\n", r.LatencyP50.Round(10*time.Microsecond))
	fmt.Fprintf(&sb, "Latency p95:   %s\n", r.LatencyP95.Round(10*time.Microsecond))
	fmt.Fprintf(&sb, "Latency p99:   %s\n", r.LatencyP99.Round(10*time.Microsecond))
	fmt.Fprintf(&sb, "Latency max:   %s\n", r.LatencyMax.Round(10*time.Microsecond))
	fmt.Fprintf(&sb, "Transferred:   %.2f MiB\n", float64(r.BytesRead)/(1<<20))
	fmt.Fprintf(&sb, "Socket errors: %d\n", r.Errors)
	var codes []int
	for c := range r.StatusCount {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Fprintf(&sb, "HTTP %d:      %d\n", c, r.StatusCount[c])
	}
	return sb.String()
}
