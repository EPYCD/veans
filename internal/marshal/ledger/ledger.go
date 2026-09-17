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

// Package ledger is Marshal's append-only record of every acquire, release
// and refusal it sees or issues. Each entry carries the sha256 of the
// previous line, so a deleted or edited line breaks the chain and Verify
// says where.
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one ledger line.
type Entry struct {
	Seq      int64          `json:"seq"`
	At       time.Time      `json:"at"`
	Action   string         `json:"action"` // acquire, release, refuse, receipt, drift, stale, allocate, free, repair
	Actor    string         `json:"actor"`  // bot username, human login, "ci", "marshal"
	ActorID  int64          `json:"actor_id,omitempty"`
	TaskID   int64          `json:"task_id,omitempty"`
	Subject  string         `json:"subject,omitempty"` // path pattern, reference id, checkout path
	Outcome  string         `json:"outcome"`           // ok, refused, error
	Reason   string         `json:"reason,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Prev     string         `json:"prev"` // sha256 of the previous line, "" for the first
	Hash     string         `json:"hash"` // sha256 of this line with Hash empty
}

// Ledger appends to one file, serialised in-process; cross-process writers
// are serialised by the O_APPEND single-write guarantee for lines under the
// pipe buffer size, which every entry here is.
type Ledger struct {
	path string
	mu   sync.Mutex
}

// Open prepares a ledger at path, creating the directory.
func Open(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ledger dir: %w", err)
	}
	return &Ledger{path: path}, nil
}

// Path returns the file the ledger writes to.
func (l *Ledger) Path() string { return l.path }

// Append writes one entry, filling Seq, At (when zero), Prev and Hash.
//
// A torn tail left by a crash is repaired first: its bytes are moved to a
// sidecar file, the ledger is cut back to its last whole entry, and a
// "repair" entry records that it happened before e is written. Without
// this, one power cut mid-write made every later Append fail -- the chain
// needs the last line's hash, and the last line was NUL padding -- and on
// 2026-09-12 that silenced the ledger for five days.
func (l *Ledger) Append(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	s, err := l.scan()
	if err != nil {
		return e, err
	}
	last := s.last()
	if len(s.torn) > 0 {
		last, err = l.repair(s)
		if err != nil {
			return e, err
		}
	}
	return l.write(last, e)
}

// write appends e chained onto last and flushes it to disk.
func (l *Ledger) write(last, e Entry) (Entry, error) {
	e.Seq = last.Seq + 1
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Prev = last.Hash
	e.Hash = ""
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	e.Hash = hashLine(line)
	line, err = json.Marshal(e)
	if err != nil {
		return e, err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return e, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return e, fmt.Errorf("append ledger: %w", err)
	}
	// Without a sync the file's new size can reach the disk before its data
	// does, and a power cut then leaves exactly the NUL tail repair exists for.
	if err := f.Sync(); err != nil {
		return e, fmt.Errorf("sync ledger: %w", err)
	}
	return e, nil
}

// repair moves the torn tail aside, truncates the ledger to its last whole
// entry and records the repair as an entry of its own.
func (l *Ledger) repair(s scanned) (Entry, error) {
	aside := fmt.Sprintf("%s.torn-%s", l.path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(aside, s.torn, 0o600); err != nil {
		return Entry{}, fmt.Errorf("save torn ledger tail: %w", err)
	}
	if err := os.Truncate(l.path, s.good); err != nil {
		return Entry{}, fmt.Errorf("truncate torn ledger tail: %w", err)
	}
	return l.write(s.last(), Entry{
		Action:  "repair",
		Actor:   "marshal",
		Subject: filepath.Base(l.path),
		Outcome: "ok",
		Reason:  "discarded a torn tail left by an interrupted write",
		Metadata: map[string]any{
			"bytes": len(s.torn),
			"nul":   bytes.Count(s.torn, []byte{0}),
			"saved": filepath.Base(aside),
			"after": s.last().Seq,
		},
	})
}

// Read returns every entry, oldest first. A torn tail is not an entry and
// is left out; Append repairs it on the next write.
func (l *Ledger) Read() ([]Entry, error) {
	s, err := l.scan()
	if err != nil {
		return s.entries, err
	}
	return s.entries, nil
}

// Verify walks the chain and returns the first sequence number whose hash or
// prev link does not match, or 0 when the ledger is intact.
func (l *Ledger) Verify() (int64, error) {
	entries, err := l.Read()
	if err != nil {
		return 0, err
	}
	prev := ""
	for _, e := range entries {
		if e.Prev != prev {
			return e.Seq, nil
		}
		want := e.Hash
		e.Hash = ""
		line, err := json.Marshal(e)
		if err != nil {
			return e.Seq, err
		}
		if hashLine(line) != want {
			return e.Seq, nil
		}
		prev = want
	}
	return 0, nil
}

// scanned is the ledger file split into whole entries and whatever follows
// the last of them.
type scanned struct {
	entries []Entry
	good    int64  // byte offset just past the last whole entry
	torn    []byte // a torn tail after good, when there is one
}

func (s scanned) last() Entry {
	if len(s.entries) == 0 {
		return Entry{}
	}
	return s.entries[len(s.entries)-1]
}

// scan reads the whole file. Parsing stops at the first line that is not an
// entry; everything from there to the end is either a torn tail, which is
// returned for repair, or corruption, which is an error.
func (l *Ledger) scan() (scanned, error) {
	s := scanned{entries: []Entry{}}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	off := 0
	var lineErr error
	for off < len(data) {
		n := bytes.IndexByte(data[off:], '\n')
		if n < 0 {
			break
		}
		var e Entry
		if err := json.Unmarshal(data[off:off+n], &e); err != nil {
			lineErr = err
			break
		}
		s.entries = append(s.entries, e)
		off += n + 1
	}
	s.good = int64(off)
	rest := data[off:]
	if len(rest) == 0 {
		return s, nil
	}
	if isTorn(rest) {
		s.torn = rest
		return s, nil
	}
	if lineErr == nil {
		lineErr = errors.New("unterminated line")
	}
	return s, fmt.Errorf("ledger line %d: %w", len(s.entries)+1, lineErr)
}

// isTorn reports whether rest -- the bytes after the last whole entry -- is
// what an interrupted write leaves behind rather than damage to the record.
//
// An interrupted write leaves NUL padding (the size reached the disk, the
// data did not) or a final line cut short before its newline. Either way no
// whole entry follows it: if one does, rest is a damaged line in the middle of
// the chain, and discarding it would destroy exactly the evidence the chain
// is there to keep. The same goes for a complete, NUL-free line that simply
// does not parse -- that is an edit, not a crash.
func isTorn(rest []byte) bool {
	if bytes.IndexByte(rest, 0) < 0 && rest[len(rest)-1] == '\n' {
		return false
	}
	// Only newline-terminated lines can be whole entries; an unterminated
	// final segment is the write that was cut short.
	lines := bytes.Split(rest, []byte{'\n'})
	for _, line := range lines[:len(lines)-1] {
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			return false
		}
	}
	return true
}

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}
