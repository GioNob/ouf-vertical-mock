package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"
)

const journalFormat = "ouf.vertical-mock-journal.v1"

// A pending record has no arrival sequence until its due time.
type record struct {
	Origin     string    `json:"origin"`
	Kind       string    `json:"kind"`
	Lateness   string    `json:"lateness,omitempty"`
	Due        time.Time `json:"due,omitempty"`
	Seq        int64     `json:"seq"`
	Collection string    `json:"collection"`
	Version    string    `json:"schemaVersion"`
	RecordedAt time.Time `json:"recordedAt"`
	ID         string    `json:"id"`
	Revision   int64     `json:"revision"`
	Op         string    `json:"op"`
	ChangedAt  time.Time `json:"changedAt"`
}

type collection struct {
	Definition   definition        `json:"definition"`
	Rate         rate              `json:"rate"`
	Removed      int64             `json:"removed"`
	RemovedDelta int64             `json:"removedDelta"`
	RetainedFrom *time.Time        `json:"retainedFrom"`
	Active       string            `json:"active"`
	Versions     map[string]schema `json:"versions"`
	Base         map[string]record `json:"base"` // latest visible revision at Floor, including tombstones
	live         map[string]record
}

type checkpoint struct {
	DomainHash  string                 `json:"domainSha256"`
	ClockFloor  time.Time              `json:"clockFloor"`
	Format      string                 `json:"format"`
	Epoch       string                 `json:"epoch"`
	Key         string                 `json:"positionKey"`
	Seed        int64                  `json:"seed"`
	Domain      domain                 `json:"domain"`
	Cycle       time.Duration          `json:"cycle"`
	Offset      time.Duration          `json:"clockOffset"`
	Operations  int64                  `json:"operations"`
	Pending     []record               `json:"pending"`
	Outages     map[string]outage      `json:"outages"`
	Floor       int64                  `json:"floor"`
	Collections map[string]*collection `json:"collections"`
	Windows     map[string]window      `json:"windows"`
}

type transaction struct {
	Observed   *time.Time      `json:"observed,omitempty"`
	Checkpoint *checkpoint     `json:"checkpoint,omitempty"`
	Rates      map[string]rate `json:"rates,omitempty"`
	Cycle      *time.Duration  `json:"cycle,omitempty"`
	Offset     *time.Duration  `json:"offset,omitempty"`
	Outage     *outage         `json:"outage,omitempty"`
	Operations int             `json:"operations,omitempty"`
	Pending    []record        `json:"pending,omitempty"`
	Released   int             `json:"released,omitempty"`
	Collection string          `json:"collection,omitempty"`
	Schema     *schema         `json:"schema,omitempty"`
	Events     []record        `json:"events,omitempty"`
	Window     *window         `json:"window,omitempty"`
}

type outage struct {
	Collection string    `json:"collection"`
	Kind       string    `json:"kind"`
	Until      time.Time `json:"until"`
	RetryAfter int64     `json:"retryAfter,omitempty"`
}

type window struct {
	Limit      int    `json:"limit"`
	Token      string `json:"token"`
	Collection string `json:"collection"`
	Low        int64  `json:"low"`
	High       int64  `json:"high"`
}

type store struct {
	sync.Mutex
	checkpoint
	cfg        config
	path       string
	history    []record
	lastRead   map[string]time.Time
	clock      func() time.Time
	at         time.Time
	lock       *os.File
	failed     error
	reschedule chan struct{}
}

func fresh(c config, d domain) (checkpoint, error) {
	key := make([]byte, 48)
	if _, err := rand.Read(key); err != nil {
		return checkpoint{}, err
	}
	cp := checkpoint{Format: journalFormat, Epoch: hex.EncodeToString(key[:16]), Key: hex.EncodeToString(key[16:]),
		Seed: c.Seed, Cycle: c.Cycle, Domain: d, DomainHash: d.fileHash, Collections: map[string]*collection{}, Windows: map[string]window{}, Outages: map[string]outage{}}
	for _, def := range d.Collections {
		sc := def.schema()
		v := sc.version()
		cp.Collections[def.Name] = &collection{Definition: def, Rate: def.Generation, Active: v, Versions: map[string]schema{v: sc}, Base: map[string]record{}}
	}
	return cp, nil
}

func (s *store) now() time.Time {
	at := s.at
	if at.IsZero() {
		at = s.clock().UTC()
	}
	now := at.Add(s.Offset)
	if now.Before(s.ClockFloor) {
		return s.ClockFloor
	}
	return now
}

func openStore(c config, clock func() time.Time) (_ *store, result error) {
	d, err := loadDomain(c.DomainFile, c.MaxLate)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(filepath.Join(c.Dir, "writer.lock"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			lock.Close()
		}
	}()
	s := &store{cfg: c, path: filepath.Join(c.Dir, "vertical-v1.jsonl"), clock: clock, lock: lock, reschedule: make(chan struct{}, 1)}
	f, err := os.OpenFile(s.path, os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.reset(d)
	}
	if err != nil {
		return nil, err
	}
	err = s.replay(f)
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	for _, c := range s.Collections {
		if err := c.Rate.validate(s.cfg.MaxLate); err != nil {
			return nil, fmt.Errorf("journal generation: %w", err)
		}
	}
	return s, s.maintain()
}

func (s *store) replay(f *os.File) error {
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			if offset == 0 {
				return fmt.Errorf("missing journal header")
			}
			if len(line) > 0 {
				// An interrupted append never published this transaction.
				if err := f.Truncate(offset); err != nil {
					return err
				}
				return f.Sync()
			}
			return nil
		}
		if err != nil {
			return err
		}
		var tx transaction
		if err := json.Unmarshal(line, &tx); err != nil {
			return fmt.Errorf("journal offset %d: %w", offset, err)
		}
		if (offset == 0) != (tx.Checkpoint != nil) {
			return fmt.Errorf("unexpected journal header at %d", offset)
		}
		if tx.Checkpoint != nil {
			cp := tx.Checkpoint
			if cp.Format != journalFormat || len(cp.Key) != 64 || len(cp.Epoch) != 32 || cp.Cycle < time.Second || cp.Windows == nil || cp.Outages == nil || len(cp.Collections) == 0 {
				return fmt.Errorf("invalid journal header")
			}
			if len(cp.DomainHash) != 64 || cp.Domain.Format != domainFormat || len(cp.Domain.Collections) != len(cp.Collections) {
				return fmt.Errorf("invalid journal domain")
			}
			for _, def := range cp.Domain.Collections {
				if cp.Collections[def.Name] == nil {
					return fmt.Errorf("missing domain collection in journal")
				}
			}
			for _, c := range cp.Collections {
				if c == nil || c.Base == nil || c.Versions[c.Active].version() != c.Active {
					return fmt.Errorf("invalid collection in journal header")
				}
			}
		} else {
			if w := tx.Window; w != nil && (s.Collections[w.Collection] == nil || w.Low > w.High || w.High > s.head()) {
				return fmt.Errorf("invalid cursor window at %d", offset)
			}
			if tx.Released < 0 || tx.Released > len(s.Pending) {
				return fmt.Errorf("invalid pending release at %d", offset)
			}
			for name := range tx.Rates {
				if s.Collections[name] == nil {
					return fmt.Errorf("unknown rate collection at %d", offset)
				}
			}
			for _, e := range tx.Pending {
				if s.Collections[e.Collection] == nil || e.Seq != 0 || e.ID == "" || !e.Due.After(e.ChangedAt) {
					return fmt.Errorf("invalid pending event at %d", offset)
				}
			}
			if tx.Schema != nil && s.Collections[tx.Collection] == nil {
				return fmt.Errorf("unknown schema collection at %d", offset)
			}
			for i, e := range tx.Events {
				c := s.Collections[e.Collection]
				if c == nil || e.Seq != s.head()+int64(i)+1 {
					return fmt.Errorf("invalid event at %d", offset)
				}
				if _, ok := c.Versions[e.Version]; !ok {
					return fmt.Errorf("unknown event schema at %d", offset)
				}
			}
		}
		s.apply(tx)
		offset += int64(len(line))
	}
}

func indexEvent(index map[string]record, e record) {
	if prev, ok := index[e.ID]; !ok || e.Revision > prev.Revision || (e.Revision == prev.Revision && e.Seq > prev.Seq) {
		index[e.ID] = e
	}
}

func (s *store) apply(tx transaction) {
	if tx.Checkpoint != nil {
		s.checkpoint = *tx.Checkpoint
		s.Domain.fileHash = s.DomainHash
		s.history = nil
		s.lastRead = map[string]time.Time{}
		for _, c := range s.Collections {
			c.live = maps.Clone(c.Base)
		}
	}
	for name, r := range tx.Rates {
		s.Collections[name].Rate = r
	}
	if tx.Cycle != nil {
		s.Cycle = *tx.Cycle
	}
	if tx.Observed != nil {
		s.ClockFloor = *tx.Observed
	}
	if tx.Offset != nil {
		s.Offset = *tx.Offset
	}
	if tx.Outage != nil {
		s.Outages[tx.Outage.Collection] = *tx.Outage
	}
	if w := tx.Window; w != nil {
		s.Windows[w.Token] = *w
	}
	s.Operations += int64(tx.Operations)
	s.Pending = s.Pending[tx.Released:]
	if len(tx.Pending) > 0 {
		s.Pending = append(s.Pending, tx.Pending...)
		slices.SortStableFunc(s.Pending, func(a, b record) int { return a.Due.Compare(b.Due) })
	}
	if tx.Schema != nil {
		c := s.Collections[tx.Collection]
		c.Active = tx.Schema.version()
		if _, ok := c.Versions[c.Active]; !ok {
			c.Versions[c.Active] = *tx.Schema
		}
	}
	for _, e := range tx.Events {
		s.history = append(s.history, e)
		indexEvent(s.Collections[e.Collection].live, e)
	}
}

func (s *store) head() int64 { return s.Floor + int64(len(s.history)) }

func (s *store) commit(tx transaction) error {
	if s.failed != nil {
		return s.failed
	}
	now := s.now()
	if tx.Offset != nil {
		now = s.at.Add(*tx.Offset)
	}
	tx.Observed = &now
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		err = json.NewEncoder(f).Encode(tx)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		s.failed = err
		return err
	}
	s.apply(tx)
	return nil
}

// Replace only after the complete checkpoint and retained suffix are durable.
func (s *store) replace(cp checkpoint, suffix []record) error {
	f, err := os.CreateTemp(filepath.Dir(s.path), ".vertical-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	err = enc.Encode(transaction{Checkpoint: &cp})
	for i := 0; err == nil && i < len(suffix); i += 1024 {
		err = enc.Encode(transaction{Events: suffix[i:min(i+1024, len(suffix))]})
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(s.path))
		if err == nil {
			err = errors.Join(dir.Sync(), dir.Close())
		}
		if err != nil {
			s.failed = err
			return err
		}
	}
	return nil
}

func (s *store) reset(d domain) error {
	cp, err := fresh(s.cfg, d)
	if err != nil {
		return err
	}
	if s.Key != "" {
		cp.Key, cp.Offset, cp.ClockFloor = s.Key, s.Offset, s.now()
	}
	if err := s.replace(cp, nil); err != nil {
		return err
	}
	s.apply(transaction{Checkpoint: &cp})
	return nil
}

func (c *collection) offersEvent(e record) bool {
	return e.Op != "DELETE" || c.Definition.Delta.Deletions == "EXPLICIT"
}

func (s *store) release(tx *transaction, now time.Time) {
	for _, e := range s.Pending {
		if now.Before(e.Due) {
			break
		}
		e.RecordedAt = e.Due
		e.Version = s.Collections[e.Collection].Active
		e.Seq = s.head() + int64(len(tx.Events)) + 1
		tx.Events = append(tx.Events, e)
		tx.Released++
	}
}

func (s *store) maintain() error {
	var tx transaction
	s.release(&tx, s.now())
	if tx.Released > 0 {
		if err := s.commit(tx); err != nil {
			return err
		}
	}
	return s.compact()
}

func (s *store) compact() error {
	if s.failed != nil {
		return s.failed
	}
	cutoff := s.now().Add(-s.cfg.Retention)
	n := 0
	for n < len(s.history) && s.history[n].RecordedAt.Before(cutoff) {
		n++
	}
	if n == 0 {
		return nil
	}
	cp := s.checkpoint
	cp.Collections = make(map[string]*collection, len(s.Collections))
	for name, c := range s.Collections {
		clone := *c
		clone.Base = maps.Clone(c.Base)
		cp.Collections[name] = &clone
	}
	for _, e := range s.history[:n] {
		c := cp.Collections[e.Collection]
		indexEvent(c.Base, e)
		c.Removed = e.Seq
		if c.offersEvent(e) {
			c.RemovedDelta = e.Seq
			t := e.ChangedAt.Add(time.Nanosecond)
			if c.RetainedFrom == nil || t.After(*c.RetainedFrom) {
				c.RetainedFrom = &t
			}
		}
	}
	cp.Floor += int64(n)
	cp.Windows = maps.Clone(s.Windows)
	for k, w := range cp.Windows {
		if w.Low < cp.Collections[w.Collection].RemovedDelta {
			delete(cp.Windows, k)
		}
	}
	suffix := s.history[n:]
	if err := s.replace(cp, suffix); err != nil {
		return err
	}
	s.checkpoint = cp
	s.history = append([]record(nil), suffix...)
	return nil
}
