package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"
)

type injection struct {
	Collection string `json:"collection"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Count      int    `json:"count"`
	Minutes    int    `json:"minutes"`
}

// Plan the whole batch before appending. Selection uses sorted IDs so replay
// and restart do not depend on Go map iteration or a process-local PRNG.
func (s *store) plan(name string, first int64, now time.Time, in injection) ([]record, error) {
	c := s.Collections[name]
	live := maps.Clone(c.live)
	ids := slices.Sorted(maps.Keys(live))
	batch := make([]record, 0, in.Count)
	for i := 0; i < in.Count; i++ {
		seq := first + int64(i)
		draw := sample(s.Seed, name, seq)
		kind := in.Kind
		if kind == "" {
			choice := int(draw % 100)
			switch {
			case len(ids) == 0 || choice < s.Rate.Create:
				kind = "create"
			case choice < s.Rate.Create+s.Rate.Update:
				kind = "update"
			default:
				kind = "delete"
			}
		}
		e := record{Seq: seq, Collection: name, Version: c.Active, RecordedAt: now, ChangedAt: now, Revision: 1, Op: "UPSERT"}
		if kind == "create" {
			e.ID = fmt.Sprintf("%s-%06d", map[string]string{"edicole": "E", "cantieri": "C"}[name], seq)
			ids = append(ids, e.ID)
		} else {
			if len(ids) == 0 {
				return nil, fmt.Errorf("collection has no live target")
			}
			pos := int((draw / 100) % uint64(len(ids)))
			if in.ID != "" {
				pos = slices.Index(ids, in.ID)
				if pos < 0 {
					return nil, fmt.Errorf("target is not live")
				}
			}
			prev := live[ids[pos]]
			e.ID, e.Revision = prev.ID, prev.Revision+1
			if !e.ChangedAt.After(prev.ChangedAt) {
				e.ChangedAt = prev.ChangedAt.Add(time.Nanosecond)
			}
			switch kind {
			case "duplicate":
				e.Revision, e.ChangedAt = prev.Revision, prev.ChangedAt
			case "late":
				e.ChangedAt = now.Add(-time.Duration(1+draw%uint64(in.Minutes)) * time.Minute)
			case "delete":
				e.Op = "DELETE"
				ids[pos] = ids[len(ids)-1]
				ids = ids[:len(ids)-1]
			}
		}
		indexEvent(live, e)
		batch = append(batch, e)
	}
	return batch, nil
}

func (s *store) cycle() error {
	if err := s.compact(); err != nil {
		return err
	}
	if s.Rate.Count == 0 {
		return nil
	}
	now := s.now().UTC()
	var all []record
	for _, name := range slices.Sorted(maps.Keys(s.Collections)) {
		batch, err := s.plan(name, s.head()+int64(len(all))+1, now, injection{Count: s.Rate.Count})
		if err != nil {
			return err
		}
		all = append(all, batch...)
	}
	return s.commit(transaction{Events: all})
}

func (s *store) schedule(ctx context.Context) {
	for {
		s.Lock()
		interval := s.Rate.Interval
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
