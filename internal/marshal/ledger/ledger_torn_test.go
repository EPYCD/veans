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

package ledger

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// twoEntries returns a ledger holding two chained entries and its raw bytes.
func twoEntries(t *testing.T) (*Ledger, []byte) {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "marshal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"acquire", "release"} {
		if _, err := l.Append(Entry{Action: a, Actor: "bot", Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	return l, raw
}

func writeRaw(t *testing.T, l *Ledger, b []byte) {
	t.Helper()
	if err := os.WriteFile(l.Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLedger_TornTailIsRepaired pins 2026-09-12: a power cut left 354 NUL
// bytes after the last entry, and every Append after that failed reading the
// last line's hash. The ledger was silent for five days.
func TestLedger_TornTailIsRepaired(t *testing.T) {
	tails := map[string][]byte{
		"nul padding":       bytes.Repeat([]byte{0}, 354),
		"line cut short":    []byte(`{"seq":3,"at":"2026-09-12T11:43:41Z","act`),
		"cut short and nul": append([]byte(`{"seq":3,"at`), bytes.Repeat([]byte{0}, 40)...),
		"whole line of nul": append(bytes.Repeat([]byte{0}, 64), '\n'),
	}
	for name, tail := range tails {
		t.Run(name, func(t *testing.T) {
			l, raw := twoEntries(t)
			writeRaw(t, l, append(append([]byte{}, raw...), tail...))

			entries, err := l.Read()
			if err != nil || len(entries) != 2 {
				t.Fatalf("read with a torn tail: %v, %d entries, want 2", err, len(entries))
			}

			got, err := l.Append(Entry{Action: "acquire", Actor: "bot", Outcome: "ok"})
			if err != nil {
				t.Fatalf("append after a torn tail: %v", err)
			}

			entries, err = l.Read()
			if err != nil || len(entries) != 4 {
				t.Fatalf("read after repair: %v, %d entries, want 4", err, len(entries))
			}
			repair := entries[2]
			if repair.Action != "repair" || repair.Seq != 3 {
				t.Fatalf("entry 3 = %s/%d, want the repair record", repair.Action, repair.Seq)
			}
			if repair.Metadata["bytes"] != float64(len(tail)) {
				t.Errorf("repair records %v bytes, want %d", repair.Metadata["bytes"], len(tail))
			}
			if got.Seq != 4 || got.Prev != repair.Hash {
				t.Errorf("appended entry seq=%d prev=%s, want seq 4 chained to the repair", got.Seq, got.Prev)
			}
			if bad, err := l.Verify(); err != nil || bad != 0 {
				t.Errorf("chain after repair: broken at %d, %v", bad, err)
			}

			after, _ := os.ReadFile(l.Path())
			if bytes.IndexByte(after, 0) >= 0 {
				t.Error("the ledger still holds NUL bytes after repair")
			}
			saved, _ := filepath.Glob(l.Path() + ".torn-*")
			if len(saved) != 1 {
				t.Fatalf("want the torn tail saved to one sidecar, found %v", saved)
			}
			if b, _ := os.ReadFile(saved[0]); !bytes.Equal(b, tail) {
				t.Error("the sidecar does not hold the discarded bytes")
			}
		})
	}
}

// TestLedger_DamageIsNotRepaired keeps the chain's purpose: anything that is
// not the leftovers of an interrupted write stays an error and the file is
// left untouched, so no evidence is discarded.
func TestLedger_DamageIsNotRepaired(t *testing.T) {
	_, raw := twoEntries(t)
	lines := bytes.SplitAfter(raw, []byte{'\n'})

	cases := map[string][]byte{
		// An edited line in the middle, with whole entries after it.
		"damaged middle line": bytes.Join([][]byte{lines[0], []byte("not json\n"), lines[1]}, nil),
		// A complete last line with no NULs that does not parse: an edit.
		"edited last line": append(append([]byte{}, lines[0]...), []byte("{\"seq\":2,\"oops}\n")...),
		// NULs followed by a whole entry: not a tail at all.
		"nul before an entry": bytes.Join([][]byte{lines[0], bytes.Repeat([]byte{0}, 32), []byte("\n"), lines[1]}, nil),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			l, _ := twoEntries(t)
			writeRaw(t, l, content)

			if _, err := l.Read(); err == nil {
				t.Error("read accepted a damaged ledger")
			}
			if _, err := l.Append(Entry{Action: "acquire", Actor: "bot", Outcome: "ok"}); err == nil {
				t.Error("append wrote onto a damaged ledger")
			}
			after, _ := os.ReadFile(l.Path())
			if !bytes.Equal(after, content) {
				t.Error("a damaged ledger was modified")
			}
			if saved, _ := filepath.Glob(l.Path() + ".torn-*"); len(saved) != 0 {
				t.Errorf("damage was treated as a torn tail: %v", saved)
			}
		})
	}
}
