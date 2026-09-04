// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Package discord posts pipeline events to a channel through a Discord
// incoming webhook: no bot token, no gateway, one POST per event, the same
// shape Linear's integration uses (an embed with a title, a link, a colour
// and a few fields).
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Embed colours, Discord's decimal RGB.
const (
	ColourClaimed  = 0x3B82F6 // blue
	ColourReview   = 0xA855F7 // purple
	ColourDone     = 0x22C55E // green
	ColourFailed   = 0xEF4444 // red
	ColourWarning  = 0xF59E0B // amber
	ColourReleased = 0x64748B // slate
	ColourInfo     = 0x0EA5E9 // sky
)

// Field is one embed field.
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Embed is one card in the message.
type Embed struct {
	Title       string  `json:"title,omitempty"`
	URL         string  `json:"url,omitempty"`
	Description string  `json:"description,omitempty"`
	Colour      int     `json:"color,omitempty"`
	Fields      []Field `json:"fields,omitempty"`
	Footer      *Footer `json:"footer,omitempty"`
	Timestamp   string  `json:"timestamp,omitempty"`
	Author      *Author `json:"author,omitempty"`
}

// Footer is the small line under an embed.
type Footer struct {
	Text string `json:"text"`
}

// Author is the line above the title, used for the actor.
type Author struct {
	Name string `json:"name"`
}

// Message is the webhook body.
type Message struct {
	Username  string  `json:"username,omitempty"`
	AvatarURL string  `json:"avatar_url,omitempty"`
	Content   string  `json:"content,omitempty"`
	Embeds    []Embed `json:"embeds,omitempty"`
}

// Notifier posts to one channel webhook.
type Notifier struct {
	URL        string
	HTTPClient *http.Client
	// Now is overridable for tests.
	Now func() time.Time

	// Presentation comes from the board and can change between polls, while
	// Send runs from both the watch loop and the webhook receiver — hence the
	// mutex rather than plain fields.
	mu        sync.RWMutex
	username  string
	avatarURL string
	events    map[string]bool
}

// New returns a notifier; an empty url makes Send a no-op so callers need
// not branch on whether Discord is configured.
func New(url, username, avatar string) *Notifier {
	n := &Notifier{
		URL:        url,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		Now:        time.Now,
	}
	n.SetPresentation(username, avatar, "")
	return n
}

// SetPresentation replaces the name, avatar and event filter the notifier
// posts with. Empty values mean "unset": no name override, no avatar, and no
// filter, which posts everything. events is the board's comma-separated form.
func (n *Notifier) SetPresentation(username, avatar, events string) {
	if n == nil {
		return
	}
	set := map[string]bool{}
	for _, e := range strings.Split(events, ",") {
		if e = strings.TrimSpace(e); e != "" {
			set[e] = true
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.username, n.avatarURL = username, avatar
	if len(set) == 0 {
		n.events = nil
		return
	}
	n.events = set
}

// Allows reports whether an event of this kind should be posted. An empty
// filter allows everything, and an empty kind is always allowed so a caller
// that has no name for its card is never silently dropped.
func (n *Notifier) Allows(kind string) bool {
	if n == nil {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.events) == 0 || kind == "" {
		return true
	}
	return n.events[kind]
}

// Enabled reports whether a webhook is configured.
func (n *Notifier) Enabled() bool { return n != nil && n.URL != "" }

// Send posts one message. A 429 is retried once after Discord's retry_after;
// any other non-2xx is an error naming the status.
func (n *Notifier) Send(ctx context.Context, m Message) error {
	if !n.Enabled() {
		return nil
	}
	n.mu.RLock()
	username, avatar := n.username, n.avatarURL
	n.mu.RUnlock()
	if m.Username == "" {
		m.Username = username
	}
	if m.AvatarURL == "" {
		m.AvatarURL = avatar
	}
	for i := range m.Embeds {
		if m.Embeds[i].Timestamp == "" {
			m.Embeds[i].Timestamp = n.Now().UTC().Format(time.RFC3339)
		}
		m.Embeds[i] = truncate(m.Embeds[i])
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		status, retryAfter, err := n.post(ctx, body)
		if err != nil {
			return err
		}
		if status == http.StatusTooManyRequests && attempt == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter):
			}
			continue
		}
		if status < 200 || status > 299 {
			return fmt.Errorf("discord webhook: HTTP %d", status)
		}
		return nil
	}
	return errors.New("discord webhook: rate limited twice")
}

func (n *Notifier) post(ctx context.Context, body []byte) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL+"?wait=true", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "marshal (workman)")
	resp, err := n.HTTPClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("discord webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	retry := 2 * time.Second
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			retry = time.Duration(secs * float64(time.Second))
		}
	}
	return resp.StatusCode, retry, nil
}

// Discord's limits: title 256, description 4096, field name 256, field
// value 1024, 25 fields.
func truncate(e Embed) Embed {
	e.Title = clip(e.Title, 256)
	e.Description = clip(e.Description, 4096)
	if len(e.Fields) > 25 {
		e.Fields = e.Fields[:25]
	}
	for i := range e.Fields {
		e.Fields[i].Name = clip(e.Fields[i].Name, 256)
		e.Fields[i].Value = clip(e.Fields[i].Value, 1024)
		if e.Fields[i].Value == "" {
			e.Fields[i].Value = "—"
		}
	}
	return e
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
