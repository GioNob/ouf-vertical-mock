package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

func payload(raw []byte, dest any) bool {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dest) == nil && d.Decode(new(any)) == io.EOF
}

type rateEdit struct {
	Count     *int    `json:"count"`
	Interval  *string `json:"interval"`
	Create    *int    `json:"create"`
	Update    *int    `json:"update"`
	Delete    *int    `json:"delete"`
	ReadDelay *string `json:"readDelay"`
}

func (r rateEdit) merge(current rate) (rate, bool) {
	for _, p := range []struct{ dst, src *int }{
		{&current.Count, r.Count}, {&current.Create, r.Create}, {&current.Update, r.Update}, {&current.Delete, r.Delete},
	} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	for _, p := range []struct {
		dst *time.Duration
		src *string
	}{{&current.Interval, r.Interval}, {&current.ReadDelay, r.ReadDelay}} {
		if p.src != nil {
			v, err := time.ParseDuration(*p.src)
			if err != nil {
				return current, false
			}
			*p.dst = v
		}
	}
	return current, current.valid()
}

func (s *store) expected(events []record) []object {
	out := make([]object, 0, len(events))
	for _, e := range events {
		it := render(s.Seed, e, s.Collections[e.Collection].Versions[e.Version])
		out = append(out, object{"sequence": e.Seq, "recordedAt": e.RecordedAt, "collection": e.Collection, "schemaVersion": e.Version, "item": it})
	}
	return out
}

func (s *store) admin(r *http.Request, path []string, raw []byte) (any, string, *problem) {
	v := s.catalogVersion()
	if len(path) != 2 {
		return nil, v, &problem{404, "NOT_FOUND"}
	}
	if path[1] == "expected" {
		if r.Method != "GET" {
			return nil, v, &problem{405, "METHOD_NOT_ALLOWED"}
		}
		since := s.Floor
		if value := r.URL.Query().Get("since"); value != "" {
			var err error
			since, err = strconv.ParseInt(value, 10, 64)
			if err != nil || since < 0 || since > s.head() {
				return nil, v, badPosition
			}
			if since < s.Floor {
				return nil, v, expired
			}
		}
		return object{"schemaVersion": v, "expected": s.expected(s.history[since-s.Floor:]), "nextSince": s.head()}, v, nil
	}
	if r.Method != "POST" {
		return nil, v, &problem{405, "METHOD_NOT_ALLOWED"}
	}
	invalid := &problem{400, "INVALID_COMMAND"}
	var tx transaction
	var collectionName string
	switch path[1] {
	case "rate":
		var e rateEdit
		if !payload(raw, &e) {
			return nil, v, invalid
		}
		next, ok := e.merge(s.Rate)
		if !ok {
			return nil, v, invalid
		}
		tx.Rate = &next
	case "outage":
		var e struct {
			Duration string `json:"duration"`
		}
		if !payload(raw, &e) {
			return nil, v, invalid
		}
		duration, err := time.ParseDuration(e.Duration)
		if err != nil || duration < 0 {
			return nil, v, invalid
		}
		until := s.now().UTC().Add(duration)
		tx.Outage = &until
	case "inject":
		e := injection{Count: 1}
		if !payload(raw, &e) || s.Collections[e.Collection] == nil {
			return nil, v, invalid
		}
		collectionName, v = e.Collection, s.Collections[e.Collection].Active
		if e.Count < 1 || e.Count > 10000 || (e.Kind != "duplicate" && e.Kind != "late" && e.Kind != "delete") ||
			(e.Kind == "late" && (e.Minutes <= 0 || int64(e.Minutes) > int64(s.cfg.Retention/time.Minute))) {
			return nil, v, invalid
		}
		var err error
		tx.Events, err = s.plan(e.Collection, s.head()+1, s.now().UTC(), e)
		if err != nil {
			return nil, v, &problem{409, "NO_LIVE_TARGET"}
		}
	case "schema":
		var e schemaEdit
		if !payload(raw, &e) || s.Collections[e.Collection] == nil {
			return nil, v, invalid
		}
		c := s.Collections[e.Collection]
		collectionName, v = e.Collection, c.Active
		var next schema
		if e.Action == "revert" {
			var ok bool
			if next, ok = c.Versions[e.Version]; !ok {
				return nil, v, invalid
			}
		} else {
			var err error
			next, err = editSchema(c.Versions[c.Active], e)
			if err != nil {
				return nil, v, invalid
			}
		}
		tx.Collection, tx.Schema = e.Collection, &next
	case "reset":
		if len(raw) > 0 && !payload(raw, &struct{}{}) {
			return nil, v, invalid
		}
		if err := s.reset(); err != nil {
			return nil, v, unavailable
		}
	default:
		return nil, v, &problem{404, "NOT_FOUND"}
	}
	if path[1] != "reset" {
		if err := s.commit(tx); err != nil {
			return nil, v, unavailable
		}
	}
	if path[1] == "reset" || path[1] == "rate" {
		select {
		case s.reschedule <- struct{}{}:
		default:
		}
	}
	v = s.catalogVersion()
	if collectionName != "" {
		v = s.Collections[collectionName].Active
	}
	return object{"schemaVersion": v, "expected": s.expected(tx.Events), "nextSince": s.head()}, v, nil
}
