package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"time"
)

func payload(b []byte, v any) bool {
	raw := bytes.TrimSpace(b)
	return len(raw) > 0 && raw[0] == '{' && strictJSON(raw, v) == nil
}

type rateEdit struct {
	Collection  string  `json:"collection"`
	Count       *int    `json:"count"`
	Create      *int    `json:"create"`
	Update      *int    `json:"update"`
	Delete      *int    `json:"delete"`
	Duplicate   *int    `json:"duplicate"`
	Late        *int    `json:"late"`
	MaxLateness *string `json:"maxLateness"`
	Cycle       *string `json:"cycle"`
}

func (s *store) setRate(tx *transaction, raw []byte) *problem {
	var e rateEdit
	if !payload(raw, &e) {
		return invalid
	}
	if e.Cycle != nil {
		d, err := duration(*e.Cycle)
		if err != nil || d < time.Second {
			return invalid
		}
		tx.Cycle = &d
	}
	tx.Rates = map[string]rate{}
	for name, c := range s.Collections {
		if e.Collection != "" && e.Collection != name {
			continue
		}
		r := c.Rate
		for _, pair := range [][2]*int{{&r.Count, e.Count}, {&r.Create, e.Create}, {&r.Update, e.Update}, {&r.Delete, e.Delete}, {&r.Duplicate, e.Duplicate}, {&r.Late, e.Late}} {
			if pair[1] != nil {
				*pair[0] = *pair[1]
			}
		}
		if e.MaxLateness != nil {
			r.MaxLateness = *e.MaxLateness
		}
		if r.validate(s.cfg.MaxLate) != nil {
			return invalid
		}
		d, _ := duration(r.MaxLateness)
		for _, p := range s.Pending {
			if p.Collection == name && p.Due.Sub(p.ChangedAt) > d {
				return &problem{409, "TARGET_NOT_AVAILABLE"}
			}
		}
		tx.Rates[name] = r
	}
	return nil
}

func (s *store) status() object {
	rates, retained, pending := map[string]rate{}, object{}, map[string]int{}
	for name, c := range s.Collections {
		rates[name], retained[name], pending[name] = c.Rate, c.RetainedFrom, 0
	}
	for _, e := range s.Pending {
		pending[e.Collection]++
	}
	activeOutages := map[string]outage{}
	for name, o := range s.Outages {
		if s.now().Before(o.Until) {
			activeOutages[name] = o
		}
	}
	var fileHash any
	if b, err := os.ReadFile(s.cfg.DomainFile); err == nil {
		fileHash = hashBytes(b)
	}
	return object{"schemaVersion": s.catalogVersion(), "journalFormat": s.Format, "domainFileSha256": fileHash, "journalDomainSha256": s.DomainHash,
		"generation": rates, "pending": len(s.Pending), "pendingByCollection": pending, "outages": activeOutages, "head": s.head(), "floor": s.Floor,
		"retainedFrom": retained, "clockOffset": iso(s.Offset), "cycle": iso(s.Cycle), "retention": iso(s.cfg.Retention), "maxLateness": iso(s.cfg.MaxLate),
		"readMinInterval": iso(s.cfg.ReadInterval), "clockControl": s.cfg.ClockControl, "port": s.cfg.Port, "seed": s.Seed, "dataDir": s.cfg.Dir, "domainFile": s.cfg.DomainFile}
}

func (s *store) admin(r *http.Request, raw []byte, name string) (any, *problem) {
	path := r.URL.Path
	if path == "/admin/status" || path == "/admin/expected" || path == "/admin/expected/state" {
		if r.Method != "GET" {
			return nil, badMethod
		}
		if path == "/admin/status" {
			if name != "" {
				return nil, badPosition
			}
			return s.status(), nil
		}
		return s.ledger(r.URL.Query(), name, path == "/admin/expected/state")
	}
	if path == "/admin/clock" && !s.cfg.ClockControl {
		return nil, notFound
	}
	if r.Method != "POST" {
		return nil, badMethod
	}
	var tx transaction
	reply := object{}
	switch path {
	case "/admin/rate":
		if prob := s.setRate(&tx, raw); prob != nil {
			return nil, prob
		}
	case "/admin/outage":
		var e struct {
			Collection, Kind, Duration string
			RetryAfter                 json.RawMessage
		}
		if !payload(raw, &e) || (e.Kind != "RATE_LIMITED" && e.Kind != "UNAVAILABLE") {
			return nil, invalid
		}
		d, err := duration(e.Duration)
		if err != nil {
			return nil, invalid
		}
		o := outage{Collection: name, Kind: e.Kind, Until: s.now().Add(d)}
		if e.RetryAfter != nil {
			var interval string
			if json.Unmarshal(e.RetryAfter, &interval) == nil {
				d, err := duration(interval)
				if err != nil || d == 0 {
					return nil, invalid
				}
				o.RetryAfter = seconds(d)
			} else if json.Unmarshal(e.RetryAfter, &o.RetryAfter) != nil || o.RetryAfter < 1 {
				return nil, invalid
			}
		}
		tx.Outage = &o
	case "/admin/inject":
		e := injection{Count: 1}
		if !payload(raw, &e) || name == "" || e.Count < 1 || e.Count > 10000 ||
			!slices.Contains([]string{"create", "update", "delete", "late", "duplicate"}, e.Kind) {
			return nil, invalid
		}
		limit, _ := duration(s.Collections[name].Rate.MaxLateness)
		if e.Kind == "late" {
			if e.Lateness == "" {
				e.Lateness = iso(limit)
			}
			d, err := duration(e.Lateness)
			if err != nil || d < time.Second || d > limit {
				return nil, invalid
			}
		} else if e.Lateness != "" {
			return nil, invalid
		}
		if err := s.plan(&tx, e, s.now()); err != nil {
			return nil, &problem{409, "TARGET_NOT_AVAILABLE"}
		}
	case "/admin/schema":
		var e schemaEdit
		if !payload(raw, &e) || name == "" {
			return nil, invalid
		}
		c := s.Collections[name]
		next, ok := c.Versions[e.Version]
		if e.Action == "revert" {
			if !ok {
				return nil, &problem{400, "UNKNOWN_SCHEMA_VERSION"}
			}
		} else {
			var err error
			next, err = editSchema(c.Versions[c.Active], e)
			if err != nil {
				return nil, invalid
			}
		}
		tx.Collection, tx.Schema = name, &next
		reply["previousSchemaVersion"], reply["fieldsSha256"] = c.Active, digest(next.Fields)
	case "/admin/clock":
		var e struct{ Advance string }
		if !payload(raw, &e) {
			return nil, invalid
		}
		d, err := duration(e.Advance)
		effective := s.now().Sub(s.at)
		if err != nil || d <= 0 || d > 1<<63-1-effective {
			return nil, invalid
		}
		offset := effective + d
		tx.Offset = &offset
		s.release(&tx, s.now().Add(d))
	case "/admin/reset":
		var e struct{ ReloadDomain bool }
		if len(raw) != 0 && !payload(raw, &e) {
			return nil, invalid
		}
		d := s.Domain
		if e.ReloadDomain {
			var err error
			d, err = loadDomain(s.cfg.DomainFile, s.cfg.MaxLate)
			if err != nil {
				return nil, invalid
			}
		}
		if err := s.reset(d); err != nil {
			return nil, unavailable
		}
	default:
		return nil, notFound
	}
	if path != "/admin/reset" {
		if err := s.commit(tx); err != nil {
			return nil, unavailable
		}
	}
	if tx.Offset != nil && s.compact() != nil {
		return nil, unavailable
	}
	if tx.Cycle != nil || path == "/admin/reset" {
		select {
		case s.reschedule <- struct{}{}:
		default:
		}
	}
	reply["schemaVersion"], reply["nextSince"] = s.version(name), s.head()
	rows := make([]object, 0, len(tx.Events))
	for _, e := range tx.Events {
		rows = append(rows, s.row(e, e.Version))
	}
	reply["rows"] = rows
	return reply, nil
}
