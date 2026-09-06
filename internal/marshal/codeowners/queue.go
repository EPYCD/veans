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

package codeowners

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"code.vikunja.io/veans/internal/marshal/pathpattern"
)

// Claim is one task's hold on a path (declared scope or live lease).
//
// The json tags are load-bearing: this crosses the wire to the board's marshal
// panel, which reads snake_case (frontend/src/client/marshal.ts). Without them
// Go emits its own field names and every rendered row comes out blank — a
// mismatch that stayed invisible only because the queues these sit in were
// always empty, so the panel showed its "no chokepoints" note instead.
type Claim struct {
	TaskID     int64     `json:"task_id"`
	Identifier string    `json:"identifier"` // "CY-12"
	Title      string    `json:"title"`
	Assignee   string    `json:"assignee"` // display name or ""
	Pattern    string    `json:"pattern"`
	Since      time.Time `json:"since"`
	Active     bool      `json:"active"` // true when a lease is held right now (in progress), false when only declared
	BlockedBy  []int64   `json:"blocked_by"`
}

// QueueEntry is one task's position on a chokepoint.
type QueueEntry struct {
	Position  int     `json:"position"` // 1 = holds it or is next
	Claim     Claim   `json:"claim"`
	WaitingOn []int64 `json:"waiting_on"` // task ids that must finish first (holders ahead + its own open blockers that are also on the queue)
}

// Queue is the ordered line of tasks on one chokepoint.
type Queue struct {
	Chokepoint string       `json:"chokepoint"`
	Entries    []QueueEntry `json:"entries"`
}

// Queues orders every claim that overlaps a chokepoint (using
// pathpattern.Overlap): active holders first (oldest Since first), then
// declared-only claims in an order that respects BlockedBy (a task never
// precedes one it is blocked by; ties by Since then TaskID). A chokepoint
// nobody claims yields an empty Queue (still returned). A task with several
// overlapping claims appears once, represented by its strongest one.
func Queues(chokepoints []string, claims []Claim) []Queue {
	out := make([]Queue, 0, len(chokepoints))
	for _, cp := range chokepoints {
		var holders, declared []Claim
		for _, c := range dedupeByTask(overlapping(cp, claims)) {
			if c.Active {
				holders = append(holders, c)
			} else {
				declared = append(declared, c)
			}
		}
		slices.SortFunc(holders, compareClaims)
		ordered := slices.Concat(holders, topoOrder(declared))
		out = append(out, Queue{Chokepoint: cp, Entries: entries(ordered)})
	}
	return out
}

func overlapping(chokepoint string, claims []Claim) []Claim {
	var out []Claim
	for _, c := range claims {
		if pathpattern.Overlap(chokepoint, c.Pattern) {
			out = append(out, c)
		}
	}
	return out
}

// dedupeByTask keeps one claim per task: an active lease beats a declaration,
// then the older claim wins.
func dedupeByTask(claims []Claim) []Claim {
	best := map[int64]Claim{}
	var order []int64
	for _, c := range claims {
		cur, ok := best[c.TaskID]
		if !ok {
			order = append(order, c.TaskID)
			best[c.TaskID] = c
			continue
		}
		if (c.Active && !cur.Active) || (c.Active == cur.Active && compareClaims(c, cur) < 0) {
			best[c.TaskID] = c
		}
	}
	out := make([]Claim, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out
}

func compareClaims(a, b Claim) int {
	if c := a.Since.Compare(b.Since); c != 0 {
		return c
	}
	if c := cmp.Compare(a.TaskID, b.TaskID); c != 0 {
		return c
	}
	return strings.Compare(a.Pattern, b.Pattern)
}

// topoOrder is Kahn's algorithm over the BlockedBy edges among the given
// claims, always releasing the smallest ready claim by (Since, TaskID). A
// blocked cycle cannot be ordered; its members are released in that same
// order rather than dropped so the queue still lists everyone.
func topoOrder(claims []Claim) []Claim {
	present := map[int64]bool{}
	for _, c := range claims {
		present[c.TaskID] = true
	}
	indegree := map[int64]int{}
	dependents := map[int64][]int64{}
	for _, c := range claims {
		for _, b := range c.BlockedBy {
			if b == c.TaskID || !present[b] {
				continue
			}
			indegree[c.TaskID]++
			dependents[b] = append(dependents[b], c.TaskID)
		}
	}
	remaining := slices.Clone(claims)
	slices.SortFunc(remaining, compareClaims)
	out := make([]Claim, 0, len(claims))
	for len(remaining) > 0 {
		i := slices.IndexFunc(remaining, func(c Claim) bool { return indegree[c.TaskID] == 0 })
		if i < 0 {
			i = 0 // cycle: nobody is ready, take the smallest
		}
		c := remaining[i]
		remaining = slices.Delete(remaining, i, i+1)
		out = append(out, c)
		for _, d := range dependents[c.TaskID] {
			indegree[d]--
		}
	}
	return out
}

func entries(ordered []Claim) []QueueEntry {
	onQueue := map[int64]bool{}
	for _, c := range ordered {
		onQueue[c.TaskID] = true
	}
	out := make([]QueueEntry, 0, len(ordered))
	for i, c := range ordered {
		waiting := map[int64]bool{}
		for _, ahead := range ordered[:i] {
			if ahead.Active {
				waiting[ahead.TaskID] = true
			}
		}
		for _, b := range c.BlockedBy {
			if onQueue[b] && b != c.TaskID {
				waiting[b] = true
			}
		}
		var ids []int64
		for id := range waiting {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		out = append(out, QueueEntry{Position: i + 1, Claim: c, WaitingOn: ids})
	}
	return out
}
