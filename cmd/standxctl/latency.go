package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"sort"
	"time"
)

// cmdLatency measures round-trip time to the exchange and verifies that the
// transport reuses connections.
//
// Reading DisableKeepAlives from the configuration proves nothing: a proxy, a
// server-side `Connection: close`, or an exhausted idle pool all silently turn
// every request into a fresh TCP and TLS handshake. Only httptrace can tell
// whether the connection was actually reused.
func cmdLatency(ctx context.Context, client *http.Client, baseURL string, samples int) error {
	type observation struct {
		total    time.Duration
		reused   bool
		newConn  bool
		tlsSetup time.Duration
	}

	results := make([]observation, 0, samples)
	url := baseURL + "/api/query_symbol_price?symbol=BTC-USD"

	for range samples {
		var obs observation
		var tlsStart time.Time

		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				obs.reused = info.Reused
				obs.newConn = !info.Reused
			},
			TLSHandshakeStart: func() { tlsStart = time.Now() },
			TLSHandshakeDone: func(tls.ConnectionState, error) {
				if !tlsStart.IsZero() {
					obs.tlsSetup = time.Since(tlsStart)
				}
			},
		}

		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		// The body must be drained fully, otherwise the connection cannot
		// return to the idle pool and the next request opens a new one.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		obs.total = time.Since(started)

		results = append(results, obs)
	}

	reused, handshakes := 0, 0
	durations := make([]time.Duration, 0, len(results))
	var tlsTotal time.Duration
	for _, o := range results {
		if o.reused {
			reused++
		}
		if o.tlsSetup > 0 {
			handshakes++
			tlsTotal += o.tlsSetup
		}
		durations = append(durations, o.total)
	}

	w := newTable()
	fmt.Fprintf(w, "samples\t%d\n", len(results))
	fmt.Fprintf(w, "connections reused\t%d of %d\n", reused, len(results))
	fmt.Fprintf(w, "tls handshakes\t%d\n", handshakes)
	if handshakes > 0 {
		fmt.Fprintf(w, "avg handshake cost\t%s\n", (tlsTotal / time.Duration(handshakes)).Round(time.Millisecond))
	}
	fmt.Fprintf(w, "rtt p50\t%s\n", percentile(durations, 50).Round(time.Millisecond))
	fmt.Fprintf(w, "rtt p95\t%s\n", percentile(durations, 95).Round(time.Millisecond))
	fmt.Fprintf(w, "rtt max\t%s\n", percentile(durations, 100).Round(time.Millisecond))
	if err := w.Flush(); err != nil {
		return err
	}

	switch {
	case len(results) < 2:
		return nil
	case reused == 0:
		fmt.Println("\nWARNING: no connection was reused. Every order would pay a TLS handshake.")
	case reused < len(results)-1:
		fmt.Printf("\nWARNING: only %d of %d requests reused a connection.\n", reused, len(results))
	default:
		fmt.Println("\nKeep-alive is working: the first request opens the connection, the rest reuse it.")
	}
	return nil
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	index := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
