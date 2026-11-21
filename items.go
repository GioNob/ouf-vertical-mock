package main

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type problem struct {
	status int
	code   string
}

var (
	badPosition = &problem{400, "INVALID_POSITION"}
	expired     = &problem{410, "CURSOR_EXPIRED"}
	unavailable = &problem{503, "UNAVAILABLE"}
)

type position struct {
	Epoch, Collection, Mode string
	Low, High               int64
	Offset                  int
	Tail                    bool
	Issued                  time.Time
	Since                   time.Time
	After                   string
}

var b64 = base64.RawURLEncoding

func (s *store) encode(p position) string {
	b, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, []byte(s.Epoch))
	mac.Write(b)
	return b64.EncodeToString(b) + "." + b64.EncodeToString(mac.Sum(nil))
}

func (s *store) decode(raw, name, mode string) (position, *problem) {
	var p position
	data, sig, ok := strings.Cut(raw, ".")
	b, err := b64.DecodeString(data)
	tag, tagErr := b64.DecodeString(sig)
	if !ok || err != nil || tagErr != nil || json.Unmarshal(b, &p) != nil || p.Collection != name || p.Mode != mode {
		return p, badPosition
	}
	if p.Epoch != digest(s.Epoch) {
		return p, expired
	}
	mac := hmac.New(sha256.New, []byte(s.Epoch))
	mac.Write(b)
	if !hmac.Equal(mac.Sum(nil), tag) {
		return p, badPosition
	}
	if p.Issued.Before(s.now().Add(-s.cfg.Retention)) || p.High < s.Floor || (mode != "SNAPSHOT" && p.Low < s.Floor) {
		return p, expired
	}
	return p, nil
}

type envelope struct {
	Collection       string  `json:"collection"`
	Version          string  `json:"schemaVersion"`
	Mode             string  `json:"mode"`
	Items            []item  `json:"items"`
	Next             *string `json:"next"`
	HasMore          bool    `json:"hasMore"`
	SnapshotComplete *bool   `json:"snapshotComplete"`
}

func eventOrder(a, b record) int {
	return cmp.Or(a.ChangedAt.Compare(b.ChangedAt), cmp.Compare(a.ID, b.ID),
		cmp.Compare(a.Revision, b.Revision), cmp.Compare(a.Seq, b.Seq))
}

func (s *store) records(p position) []record {
	c := s.Collections[p.Collection]
	var result []record
	if p.Mode == "SNAPSHOT" {
		index := maps.Clone(c.Base)
		for _, e := range s.history[:p.High-s.Floor] {
			if e.Collection == p.Collection {
				indexEvent(index, e)
			}
		}
		for _, e := range index {
			result = append(result, e)
		}
	} else {
		for _, e := range s.history[p.Low-s.Floor:p.High-s.Floor] {
			if e.Collection != p.Collection {
				continue
			}
			if p.Mode == "WATERMARK" && (e.ChangedAt.Before(p.Since) || (e.ChangedAt.Equal(p.Since) && e.ID <= p.After)) {
				continue
			}
			result = append(result, e)
		}
	}
	slices.SortFunc(result, eventOrder)
	return result
}

func (s *store) items(name string, q url.Values) (envelope, *problem) {
	now := s.now().UTC()
	mode := q.Get("mode")
	if mode == "" {
		mode = "CURSOR"
	}
	c := s.Collections[name]
	out := envelope{Collection: name, Version: c.Active, Mode: mode, Items: []item{}}
	limit := 100
	if q.Has("limit") {
		var err error
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 1000 {
			return out, &problem{400, "INVALID_LIMIT"}
		}
	}
	key := "cursor"
	if mode == "SNAPSHOT" {
		key = "page"
	} else if mode != "CURSOR" && mode != "WATERMARK" {
		return out, &problem{400, "INVALID_MODE"}
	}
	p := position{Epoch: digest(s.Epoch), Collection: name, Mode: mode, Low: s.Floor, High: s.head(), Issued: now}
	if (q.Has("page") && key != "page") || (q.Has("cursor") && key != "cursor") ||
		((q.Has("changedSince") || q.Has("afterId")) && (mode != "WATERMARK" || q.Has(key))) {
		return out, badPosition
	}
	if q.Has(key) {
		var prob *problem
		p, prob = s.decode(q.Get(key), name, mode)
		if prob != nil {
			return out, prob
		}
		if p.Tail {
			p.High = s.head()
			if bound, ok := s.Windows[name][p.Low]; ok {
				p.High = bound
			} else if p.High > p.Low {
				// Freeze the first non-empty poll from this checkpoint for retries.
				if err := s.commit(transaction{Window: &window{name, p.Low, p.High}}); err != nil {
					return out, unavailable
				}
			}
			p.Tail = false
		}
	} else if mode == "WATERMARK" {
		if q.Has("changedSince") {
			var err error
			p.Since, err = time.Parse(time.RFC3339Nano, q.Get("changedSince"))
			if err != nil {
				return out, badPosition
			}
			if p.Since.Before(now.Add(-s.cfg.Retention)) {
				return out, expired
			}
			p.After = q.Get("afterId")
		} else if q.Has("afterId") {
			return out, badPosition
		}
	}
	rows := s.records(p)
	end := min(p.Offset+limit, len(rows))
	for _, e := range rows[p.Offset:end] {
		out.Items = append(out.Items, render(s.Seed, e, c.Versions[c.Active]))
	}
	out.HasMore = end < len(rows)
	if mode == "SNAPSHOT" {
		complete := !out.HasMore
		out.SnapshotComplete = &complete
	}
	if out.HasMore {
		p.Offset = end
		next := s.encode(p)
		out.Next = &next
	} else if mode == "CURSOR" {
		// A retry of a populated window returns the same next token.
		// Polling an idle source renews an otherwise valid checkpoint.
		if p.High == p.Low {
			p.Issued = now
		} else if arrived := s.history[p.High-s.Floor-1].RecordedAt; arrived.After(p.Issued) {
			p.Issued = arrived
		}
		p.Low, p.Offset, p.Tail = p.High, 0, true
		next := s.encode(p)
		out.Next = &next
	}
	return out, nil
}
