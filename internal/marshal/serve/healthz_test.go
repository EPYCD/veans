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

package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"code.vikunja.io/veans/internal/marshal/board"
	"code.vikunja.io/veans/internal/marshal/config"
	"code.vikunja.io/veans/internal/marshal/engine"
	"code.vikunja.io/veans/internal/marshal/ledger"
)

func healthz(t *testing.T, s *Server) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	return rec.Code, body
}

// TestHealthzReportsAnUnwritableLedger pins the gap that hid a broken ledger
// for five days: every append failed, and /healthz -- the container
// healthcheck -- kept answering ok.
func TestHealthzReportsAnUnwritableLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	// A ledger damaged mid-chain cannot be appended to and is not repaired.
	if err := os.WriteFile(path, []byte("not json\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{
		Cfg:    &config.Config{Serve: config.Serve{AllowOrigins: []string{"https://board.example"}}},
		Board:  &board.Board{Identity: "marshal"},
		Ledger: l,
	}
	s := New(e, "origin/main", nil)

	if code, _ := healthz(t, s); code != http.StatusOK {
		t.Fatalf("before any write: status %d, want 200", code)
	}

	e.Log(ledger.Entry{Action: "health", Outcome: "ok"})
	code, body := healthz(t, s)
	if code != http.StatusServiceUnavailable || body["ok"] != false {
		t.Fatalf("after a failed ledger write: status %d ok=%v, want 503 and false", code, body["ok"])
	}
	if msg, _ := body["ledger_error"].(string); msg == "" {
		t.Error("healthz does not say what is wrong with the ledger")
	}

	// Once the ledger can be written again, health recovers on its own.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.Log(ledger.Entry{Action: "health", Outcome: "ok"})
	if code, body := healthz(t, s); code != http.StatusOK || body["ok"] != true {
		t.Fatalf("after a successful write: status %d ok=%v, want 200 and true", code, body["ok"])
	}
}
