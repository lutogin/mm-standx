// Command analyze summarises recorded market data.
//
// It answers the questions that decide the quoting geometry: how often the
// reference price actually moves, how far it jumps between ticks, and how wide
// the spread sits. These are inputs to the distance thresholds, not curiosities.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

type entry struct {
	Stream   string          `json:"stream"`
	Received int64           `json:"ts_us"`
	Payload  json.RawMessage `json:"payload"`
}

type message struct {
	Channel string `json:"channel"`
	Symbol  string `json:"symbol"`
	Data    struct {
		Symbol    string `json:"symbol"`
		MarkPrice string `json:"mark_price"`
		Bids      [][2]string
		Asks      [][2]string
	} `json:"data"`
}

// series accumulates statistics for one instrument.
type series struct {
	markCount  int
	bookCount  int
	firstAt    time.Time
	lastAt     time.Time
	lastMark   float64
	lastMarkAt time.Time

	gapsMs  []float64 // intervals between consecutive mark updates
	movesBp []float64 // absolute move between consecutive mark updates
	spreads []float64
}

func main() {
	dir := flag.String("dir", "recordings", "directory holding the recordings")
	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*dir, "*.ndjson"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no recordings found in %s\n", *dir)
		os.Exit(1)
	}
	sort.Strings(files)

	stats := map[string]*series{}
	lines := 0
	for _, name := range files {
		n, err := scan(name, stats)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			os.Exit(1)
		}
		lines += n
	}

	report(files, lines, stats)
}

func scan(name string, stats map[string]*series) (int, error) {
	f, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)

	count := 0
	for scanner.Scan() {
		count++
		var e entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		var m message
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			continue
		}

		symbol := m.Symbol
		if symbol == "" {
			symbol = m.Data.Symbol
		}
		if symbol == "" {
			continue
		}

		s := stats[symbol]
		if s == nil {
			s = &series{}
			stats[symbol] = s
		}
		at := time.UnixMicro(e.Received)
		if s.firstAt.IsZero() {
			s.firstAt = at
		}
		s.lastAt = at

		switch m.Channel {
		case "price":
			observeMark(s, m.Data.MarkPrice, at)
		case "depth_book":
			observeBook(s, m.Data.Bids, m.Data.Asks)
		}
	}
	return count, scanner.Err()
}

func observeMark(s *series, raw string, at time.Time) {
	mark, err := strconv.ParseFloat(raw, 64)
	if err != nil || mark <= 0 {
		return
	}
	s.markCount++

	if s.lastMark > 0 {
		s.gapsMs = append(s.gapsMs, float64(at.Sub(s.lastMarkAt).Microseconds())/1000)
		s.movesBp = append(s.movesBp, math.Abs(mark-s.lastMark)/s.lastMark*10_000)
	}
	s.lastMark, s.lastMarkAt = mark, at
}

func observeBook(s *series, bids, asks [][2]string) {
	s.bookCount++
	if len(bids) == 0 || len(asks) == 0 {
		return
	}

	bestBid, bestAsk := 0.0, math.MaxFloat64
	for _, l := range bids {
		if p, err := strconv.ParseFloat(l[0], 64); err == nil && p > bestBid {
			bestBid = p
		}
	}
	for _, l := range asks {
		if p, err := strconv.ParseFloat(l[0], 64); err == nil && p < bestAsk {
			bestAsk = p
		}
	}
	if bestBid > 0 && bestAsk < math.MaxFloat64 && bestAsk > bestBid {
		s.spreads = append(s.spreads, (bestAsk-bestBid)/bestBid*10_000)
	}
}

func report(files []string, lines int, stats map[string]*series) {
	symbols := make([]string, 0, len(stats))
	for s := range stats {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)

	fmt.Printf("files: %d, messages: %d\n\n", len(files), lines)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SYMBOL\tSPAN\tMARKS\tBOOKS\tMARK/s\tGAP p50\tGAP p95\tGAP max\tMOVE p50\tMOVE p95\tMOVE max\tSPREAD p50\tSPREAD p95")
	for _, symbol := range symbols {
		s := stats[symbol]
		span := s.lastAt.Sub(s.firstAt)
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%.2f\t%.0fms\t%.0fms\t%.0fms\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\n",
			symbol, span.Round(time.Minute), s.markCount, s.bookCount,
			float64(s.markCount)/span.Seconds(),
			pct(s.gapsMs, 50), pct(s.gapsMs, 95), pct(s.gapsMs, 100),
			pct(s.movesBp, 50), pct(s.movesBp, 95), pct(s.movesBp, 100),
			pct(s.spreads, 50), pct(s.spreads, 95))
	}
	w.Flush()

	fmt.Println("\nGAP  - interval between consecutive mark price updates")
	fmt.Println("MOVE - absolute mark price change between updates, bps")
	fmt.Println("SPREAD - best bid to best ask, bps")
}

func pct(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	index := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
