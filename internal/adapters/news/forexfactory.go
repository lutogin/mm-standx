// Package news reads the weekly economic calendar.
//
// The source is the ForexFactory-compatible weekly JSON published by
// Faireconomy. It is a third party with no contract to us, so everything here
// is written for the day it changes shape or stops answering: a malformed entry
// is skipped rather than fatal, and the caller decides what an unavailable
// calendar means.
package news

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/ports"
)

// DefaultURL is the weekly feed used unless the configuration names another.
const DefaultURL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"

// Client fetches and parses the weekly calendar.
type Client struct {
	url  string
	http *http.Client
}

func New(url string, httpClient *http.Client) *Client {
	if url == "" {
		url = DefaultURL
	}
	return &Client{url: url, http: httpClient}
}

// entry is one row of the feed.
//
// The currency lives under "country", which is the feed's own naming: the
// values are currency codes (USD, EUR), not countries.
type entry struct {
	Title    string `json:"title"`
	Currency string `json:"country"`
	Date     string `json:"date"`
	Impact   string `json:"impact"`
}

// Events fetches the week's releases.
//
// Entries that do not parse are dropped, not fatal. The alternative — refusing
// the whole calendar over one malformed row — would turn a cosmetic change at
// the publisher into a blind spot in front of the next release, which is the
// opposite of what this exists for.
func (c *Client) Events(ctx context.Context) ([]calendar.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("news: build request: %w", err)
	}
	// The publisher rejects requests without one.
	req.Header.Set("User-Agent", "mm-standx")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("news: fetch %s: %w", c.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news: fetch %s: status %d", c.url, resp.StatusCode)
	}

	var rows []entry
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("news: decode calendar: %w", err)
	}
	return parse(rows), nil
}

func parse(rows []entry) []calendar.Event {
	out := make([]calendar.Event, 0, len(rows))
	for _, row := range rows {
		// The feed stamps events in the publisher's local zone with an explicit
		// offset. Parsing that offset rather than assuming UTC is what keeps the
		// blackout aligned with the release across daylight saving changes.
		at, err := time.Parse(time.RFC3339, row.Date)
		if err != nil {
			continue
		}
		out = append(out, calendar.Event{
			At:       at.UTC(),
			Title:    row.Title,
			Currency: row.Currency,
			Impact:   calendar.ParseImpact(row.Impact),
		})
	}
	return out
}

var _ ports.NewsSource = (*Client)(nil)
