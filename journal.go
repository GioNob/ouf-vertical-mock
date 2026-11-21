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
	"sync"
	"time"
)

// Sequence is arrival order. ChangedAt is the source clock and may go backwards.
type record struct {
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
	Active   string            `json:"active"`
	Versions map[string]schema `json:"versions"`
	Base     map[string]record `json:"base"` // live objects immediately after Floor
	live     map[string]record
}

type checkpoint struct {
	Format      int                    `json:"format"`
	Epoch       string                 `json:"epoch"`
	Seed        int64                  `json:"seed"`
	Rate        rate                   `json:"rate"`
	OutageUntil time.Time              `json:"outageUntil"`
	Floor       int64                  `json:"floor"`
	Collections map[string]*collection `json:"collections"`
	Windows     map[string]map[int64]int64 `json:"windows"`
}

type transaction struct {
	Checkpoint *checkpoint `json:"checkpoint,omitempty"`
	Rate       *rate       `json:"rate,omitempty"`
	Outage     *time.Time  `json:"outageUntil,omitempty"`
	Collection string      `json:"collection,omitempty"`
	Schema     *schema     `json:"schema,omitempty"`
	Events     []record    `json:"events,omitempty"`
	Window     *window     `json:"window,omitempty"`
}

type window struct {
	Collection string `json:"collection"`
	Low        int64  `json:"low"`
	High       int64  `json:"high"`
}

type store struct {
	sync.Mutex
	checkpoint
	cfg       config
	path      string
	history   []record
	lastRead  map[string]time.Time
	now       func() time.Time
	failed    error
	reschedule chan struct{}
}

func fresh(c config) (checkpoint, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return checkpoint{}, err
	}
	cp := checkpoint{Format: 1, Epoch: hex.EncodeToString(key), Seed: c.Seed, Rate: c.Rate, Collections: map[string]*collection{}, Windows: map[string]map[int64]int64{}}
	for _, name := range c.Collections {
		cp.Windows[name] = map[int64]int64{}
		s := initialSchema(name)
		v := s.version()
		cp.Collections[name] = &collection{Active: v, Versions: map[string]schema{v: s}, Base: map[string]record{}}
	}
	return cp, nil
}

func openStore(c config, clock func() time.Time) (*store, error) {
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return nil, err
	}
	s := &store{cfg: c, path: filepath.Join(c.Dir, "vertical-v1.jsonl"), now: clock, reschedule: make(chan struct{}, 1)}
	f, err := os.OpenFile(s.path, os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.reset()
	}
	if err != nil {
		return nil, err
	}
	err = s.replay(f)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return s, s.compact()
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
			if cp.Format != 1 || len(cp.Epoch) != 64 || !cp.Rate.valid() || len(cp.Collections) == 0 {
				return fmt.Errorf("invalid journal header")
			}
			for name, c := range cp.Collections {
				if c == nil || c.Base == nil || cp.Windows[name] == nil || c.Versions[c.Active].version() != c.Active {
					return fmt.Errorf("invalid collection in journal header")
				}
			}
		} else {
			if w := tx.Window; w != nil && (s.Windows[w.Collection] == nil || w.Low < s.Floor || w.Low >= w.High || w.High > s.head()) {
				return fmt.Errorf("invalid cursor window at %d", offset)
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
	if e.Op == "DELETE" {
		delete(index, e.ID)
	} else {
		index[e.ID] = e
	}
}

func (s *store) apply(tx transaction) {
	if tx.Checkpoint != nil {
		s.checkpoint = *tx.Checkpoint
		s.history = nil
		s.lastRead = map[string]time.Time{}
		for _, c := range s.Collections {
			c.live = maps.Clone(c.Base)
		}
	}
	if tx.Rate != nil {
		s.Rate = *tx.Rate
	}
	if tx.Outage != nil {
		s.OutageUntil = *tx.Outage
	}
	if w := tx.Window; w != nil {
		s.Windows[w.Collection][w.Low] = w.High
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

func (s *store) reset() error {
	cp, err := fresh(s.cfg)
	if err != nil {
		return err
	}
	if err := s.replace(cp, nil); err != nil {
		return err
	}
	s.apply(transaction{Checkpoint: &cp})
	return nil
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
		indexEvent(cp.Collections[e.Collection].Base, e)
	}
	cp.Floor += int64(n)
	cp.Windows = make(map[string]map[int64]int64, len(s.Windows))
	for name, windows := range s.Windows {
		cp.Windows[name] = make(map[int64]int64)
		for low, high := range windows {
			if low >= cp.Floor {
				cp.Windows[name][low] = high
			}
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
