package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"
)

type injection struct {
	Collection string `json:"collection"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Count      int    `json:"count"`
	Lateness   string `json:"lateness"`
}

// Stable initial order and constant-time insert/remove keep a large batch
// deterministic without sorting the full collection for every operation.
type targets struct {
	ids   []string
	index map[string]int
}

func newTargets() *targets {
	return &targets{index: map[string]int{}}
}

func (t *targets) add(id string) {
	if _, exists := t.index[id]; !exists {
		t.index[id] = len(t.ids)
		t.ids = append(t.ids, id)
	}
}

func (t *targets) remove(id string) {
	i, ok := t.index[id]
	if !ok {
		return
	}
	last := len(t.ids) - 1
	t.ids[i] = t.ids[last]
	t.index[t.ids[i]] = i
	t.ids = t.ids[:last]
	delete(t.index, id)
}

func (t *targets) choose(id string, draw uint64) (string, bool) {
	if id != "" {
		_, ok := t.index[id]
		return id, ok
	}
	if len(t.ids) == 0 {
		return "", false
	}
	return t.ids[draw%uint64(len(t.ids))], true
}

// Plan against private indexes. A failed target leaves the entire command
// (including sequence allocation and the deterministic sampler) untouched.
func (s *store) plan(tx *transaction, in injection, now time.Time) error {
	c := s.Collections[in.Collection]
	visible := maps.Clone(c.live)
	latest := maps.Clone(visible)
	for _, p := range s.Pending {
		if p.Collection == in.Collection {
			indexEvent(latest, p)
		}
	}
	limit, _ := duration(c.Rate.MaxLateness)
	live, recent := newTargets(), newTargets()
	for _, id := range slices.Sorted(maps.Keys(latest)) {
		if latest[id].Op != "DELETE" {
			live.add(id)
		}
		if e, ok := visible[id]; ok && now.Sub(e.ChangedAt) <= limit {
			recent.add(id)
		}
	}
	arrive := func(e record) {
		e.RecordedAt = now
		e.Seq = s.head() + int64(len(tx.Events)) + 1
		tx.Events = append(tx.Events, e)
		indexEvent(visible, e)
		if now.Sub(visible[e.ID].ChangedAt) <= limit {
			recent.add(e.ID)
		}
	}
	for range in.Count {
		tx.Operations++
		n := s.Operations + int64(tx.Operations)
		draw := func(i int) uint64 { return sample(s.Seed, in.Collection, "", "operation", n, i) }
		kind, origin := in.Kind, "INJECTED"
		if kind == "" {
			origin = "GENERATED"
			choice := int(draw(2) % 100)
			switch {
			case len(live.ids) == 0:
				kind = "create"
			case int(draw(3)%100) < c.Rate.Late:
				kind = "late"
			case choice < c.Rate.Create:
				kind = "create"
			case choice < c.Rate.Create+c.Rate.Update:
				kind = "update"
			default:
				kind = "delete"
			}
		}
		duplicate := func(id string) bool {
			target, ok := recent.choose(id, draw(1))
			if !ok {
				return false
			}
			e := visible[target]
			e.Kind, e.Origin, e.Lateness, e.Version = "DUPLICATE", origin, "", c.Active
			arrive(e)
			return true
		}
		if kind == "duplicate" {
			if !duplicate(in.ID) {
				return fmt.Errorf("no visible target within maxLateness")
			}
			continue
		}
		e := record{Collection: in.Collection, Version: c.Active, Origin: origin, Kind: strings.ToUpper(kind),
			ChangedAt: now, ID: in.ID, Revision: 1, Op: "UPSERT"}
		if kind == "create" {
			if e.ID == "" {
				base := fmt.Sprintf("%s%06d", c.Definition.Prefix, n)
				e.ID = base
				for suffix := 1; latest[e.ID].ID != ""; suffix++ {
					e.ID = fmt.Sprintf("%s-%d", base, suffix)
				}
			}
			if prev, ok := latest[e.ID]; ok {
				if prev.Op != "DELETE" {
					return fmt.Errorf("target already exists")
				}
				e.Revision = prev.Revision + 1
			}
		} else {
			target, ok := live.choose(in.ID, draw(1))
			if !ok {
				return fmt.Errorf("no live target")
			}
			e.ID, e.Revision = target, latest[target].Revision+1
		}
		if kind == "delete" {
			e.Op = "DELETE"
			live.remove(e.ID)
		} else {
			live.add(e.ID)
		}
		indexEvent(latest, e)
		if kind == "late" {
			delay, _ := duration(in.Lateness)
			if delay == 0 {
				delay = time.Second * time.Duration(1+draw(4)%uint64(limit/time.Second))
			}
			e.Kind, e.Due, e.Lateness = "LATE_UPDATE", now.Add(delay), iso(delay)
			tx.Pending = append(tx.Pending, e)
		} else {
			arrive(e)
		}
		if origin == "GENERATED" && int(draw(5)%100) < c.Rate.Duplicate {
			duplicate("")
		}
	}
	return nil
}

func (s *store) cycle() error {
	s.at = s.clock().UTC()
	defer func() { s.at = time.Time{} }()
	if err := s.maintain(); err != nil {
		return err
	}
	var tx transaction
	for _, name := range slices.Sorted(maps.Keys(s.Collections)) {
		if err := s.plan(&tx, injection{Collection: name, Count: s.Collections[name].Rate.Count}, s.now()); err != nil {
			return err
		}
	}
	if tx.Operations == 0 {
		return nil
	}
	return s.commit(tx)
}

func (s *store) schedule(ctx context.Context) {
	for {
		s.Lock()
		interval := s.Cycle
		s.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.reschedule:
			timer.Stop()
		case <-timer.C:
			s.Lock()
			err := s.cycle()
			s.Unlock()
			if err != nil {
				log.Printf("generation: %v", err)
			}
		}
	}
}
