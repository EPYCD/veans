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

package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSend_PostsEmbedAndRetriesOn429(t *testing.T) {
	var calls int32
	var got Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Error(err)
		}
		if !strings.HasSuffix(r.URL.String(), "?wait=true") {
			t.Errorf("url %s", r.URL)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, "Marshal", "")
	n.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	err := n.Send(context.Background(), Message{Embeds: []Embed{{Title: strings.Repeat("x", 300), Fields: []Field{{Name: "Task", Value: ""}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected one retry, got %d calls", calls)
	}
	if got.Username != "Marshal" || len(got.Embeds) != 1 {
		t.Fatalf("message %+v", got)
	}
	if len([]rune(got.Embeds[0].Title)) != 256 {
		t.Fatalf("title not clipped: %d", len(got.Embeds[0].Title))
	}
	if got.Embeds[0].Fields[0].Value != "—" {
		t.Fatalf("empty field value must be filled: %q", got.Embeds[0].Fields[0].Value)
	}
	if got.Embeds[0].Timestamp != "2026-09-02T12:00:00Z" {
		t.Fatalf("timestamp %q", got.Embeds[0].Timestamp)
	}
}

func TestSend_DisabledIsNoop(t *testing.T) {
	var n *Notifier
	if err := n.Send(context.Background(), Message{Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := New("", "", "").Send(context.Background(), Message{Content: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestSend_ErrorNamesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	err := New(srv.URL, "", "").Send(context.Background(), Message{Content: "x"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected a 400 error, got %v", err)
	}
}

// The board can change how the relay presents itself between polls, so the
// filter and identity have to be replaceable on a live notifier.
func TestSetPresentation(t *testing.T) {
	n := New("https://discord.example/webhook", "Marshal", "https://avatar")

	t.Run("no filter posts everything", func(t *testing.T) {
		if !n.Allows("drift") || !n.Allows("health") {
			t.Fatal("an unset filter must allow every event")
		}
	})

	t.Run("a filter admits only what it names", func(t *testing.T) {
		n.SetPresentation("CapYard", "", "drift, stale ,,")
		if !n.Allows("drift") || !n.Allows("stale") {
			t.Error("named events must be allowed, and whitespace and empties ignored")
		}
		if n.Allows("health") {
			t.Error("an event the filter does not name must be dropped")
		}
		// A card with no name is never silently dropped.
		if !n.Allows("") {
			t.Error("an unnamed card must still post")
		}
	})

	t.Run("clearing the filter restores everything", func(t *testing.T) {
		n.SetPresentation("CapYard", "", "")
		if !n.Allows("health") {
			t.Fatal("an emptied filter must go back to allowing every event")
		}
	})

	t.Run("a nil notifier allows nothing and does not panic", func(t *testing.T) {
		var none *Notifier
		none.SetPresentation("x", "y", "z")
		if none.Allows("drift") {
			t.Fatal("a nil notifier must not claim to allow an event")
		}
	})
}
