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
	oldEpoch    = &problem{410, "CURSOR_EXPIRED"}
	unavailable = &problem{503, "SOURCE_UNAVAILABLE"}
	unknown     = &problem{404, "UNKNOWN_COLLECTION"}
	invalid     = &problem{400, "INVALID_COMMAND"}
	notFound    = &problem{404, "NOT_FOUND"}
	badMethod   = &problem{405, "METHOD_NOT_ALLOWED"}
)

type orderKey struct {
	At       time.Time
	ID       string
	Revision int64
	Seq      int64
}

func keyOf(e record) orderKey {
	return orderKey{e.ChangedAt, e.ID, e.Revision, e.Seq}
}

func (a orderKey) compare(b orderKey) int {
	return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision), cmp.Compare(a.Seq, b.Seq))
}

type position struct {
	Epoch, Collection, Mode string
	Low, High, FirstUnread  int64
	Last                    *orderKey
	Tail                    bool
	Poll                    uint64
	Limit                   int
	Since, At               time.Time
	After, MaxLateness      string
}

var b64 = base64.RawURLEncoding

func (s *store) encode(p position) string {
	b, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, []byte(s.Key))
	mac.Write(b)
	return b64.EncodeToString(b) + "." + b64.EncodeToString(mac.Sum(nil))
}

func (s *store) decode(raw, name, mode string) (position, *problem) {
	var p position
	data, sig, ok := strings.Cut(raw, ".")
	b, err := b64.DecodeString(data)
	tag, tagErr := b64.DecodeString(sig)
	mac := hmac.New(sha256.New, []byte(s.Key))
	mac.Write(b)
	if !ok || err != nil || tagErr != nil || b64.EncodeToString(b) != data || b64.EncodeToString(tag) != sig ||
		!hmac.Equal(mac.Sum(nil), tag) || json.Unmarshal(b, &p) != nil || p.Collection != name || p.Mode != mode {
		return p, badPosition
	}
	c := s.Collections[name]
	if p.Epoch != s.Epoch {
		return p, oldEpoch
	}
	if (p.FirstUnread != 0 && p.FirstUnread <= s.Floor) ||
		(p.Tail && p.Low < c.RemovedDelta) || (mode == "SNAPSHOT" && p.High < c.Removed) {
		return p, expired
	}
	return p, nil
}

type resume struct {
	Cursor     string            `json:"cursor,omitempty"`
	Watermark  map[string]string `json:"watermark,omitempty"`
	SnapshotAt time.Time         `json:"snapshotAt"`
}

type envelope struct {
	Collection        string     `json:"collection"`
	Epoch             string     `json:"epoch"`
	Version           string     `json:"schemaVersion"`
	FieldsSha256      string     `json:"fieldsSha256"`
	Mode              string     `json:"mode"`
	Items             []item     `json:"items"`
	Next              *string    `json:"next"`
	HasMore           bool       `json:"hasMore"`
	SnapshotComplete  *bool      `json:"snapshotComplete"`
	SnapshotID        *string    `json:"snapshotId"`
	SnapshotAt        *time.Time `json:"snapshotAt,omitempty"`
	Resume            *resume    `json:"resume"`
	PositionExpiresAt *time.Time `json:"positionExpiresAt"`
}

// stateAt returns a read-only index at an inclusive arrival sequence.
func (s *store) stateAt(name string, high int64) map[string]record {
	c := s.Collections[name]
	if high == s.head() {
		return c.live
	}
	index := maps.Clone(c.Base)
	for _, e := range s.history {
		if e.Seq > high {
			break
		}
		if e.Collection == name {
			indexEvent(index, e)
		}
	}
	return index
}

func (s *store) records(p position) []record {
	c := s.Collections[p.Collection]
	var rows []record
	if p.Mode == "SNAPSHOT" {
		for _, e := range s.stateAt(p.Collection, p.High) {
			if e.Op != "DELETE" || (c.offersEvent(e) && e.Seq > p.Low) {
				rows = append(rows, e)
			}
		}
	} else {
		for _, e := range s.history {
			if e.Seq > p.High {
				break
			}
			if e.Seq <= p.Low || e.Collection != p.Collection || !c.offersEvent(e) {
				continue
			}
			if p.Mode == "WATERMARK" && (e.ChangedAt.Before(p.Since) || (e.ChangedAt.Equal(p.Since) && e.ID <= p.After)) {
				continue
			}
			rows = append(rows, e)
		}
	}
	if p.Last != nil {
		rows = slices.DeleteFunc(rows, func(e record) bool { return keyOf(e).compare(*p.Last) <= 0 })
	}
	slices.SortFunc(rows, func(a, b record) int { return keyOf(a).compare(keyOf(b)) })
	return rows
}

func pageLimit(q url.Values, maximum int) (int, *problem) {
	if !q.Has("limit") {
		return 100, nil
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err != nil || n < 1 || n > maximum {
		return 0, &problem{400, "INVALID_LIMIT"}
	}
	return n, nil
}

func (s *store) afterExpiry(name string, high int64, all bool) *time.Time {
	for _, e := range s.history {
		if e.Collection == name && e.Seq > high && (all || s.Collections[name].offersEvent(e)) {
			t := e.RecordedAt.Add(s.cfg.Retention)
			return &t
		}
	}
	return nil
}

func (s *store) items(name string, q url.Values) (envelope, *problem) {
	c := s.Collections[name]
	mode := q.Get("mode")
	if !q.Has("mode") {
		mode = c.Definition.Delta.Default
	}
	sc := c.Versions[c.Active]
	out := envelope{Collection: name, Epoch: s.Epoch, Version: c.Active, FieldsSha256: digest(sc.Fields), Mode: mode, Items: []item{}}
	key := "cursor"
	if mode == "SNAPSHOT" {
		key = "page"
	} else if mode != "CURSOR" && mode != "WATERMARK" {
		return out, &problem{400, "INVALID_MODE"}
	}
	if !slices.Contains(c.Definition.Delta.Modes, mode) {
		return out, &problem{400, "MODE_NOT_OFFERED"}
	}
	limit, prob := pageLimit(q, 1000)
	if prob != nil {
		return out, prob
	}
	for k, vs := range q {
		if len(vs) != 1 || (k != "limit" && k != "mode" && k != key &&
			!(mode == "WATERMARK" && !q.Has(key) && (k == "changedSince" || k == "afterId"))) {
			return out, badPosition
		}
	}
	p := position{Epoch: s.Epoch, Collection: name, Mode: mode, Low: s.Floor, High: s.head(), At: s.now(), MaxLateness: c.Rate.MaxLateness}
	if q.Has(key) {
		token := q.Get(key)
		p, prob = s.decode(token, name, mode)
		if prob != nil {
			return out, prob
		}
		if p.Tail {
			id := digest(token)
			if w, ok := s.Windows[id]; ok {
				p.High, p.Limit = w.High, w.Limit
			} else {
				p.High, p.Limit = s.head(), limit
				w := window{Token: id, Collection: name, Low: p.Low, High: p.High, Limit: limit}
				if err := s.commit(transaction{Window: &w}); err != nil {
					return out, unavailable
				}
			}
			p.Tail = false
		}
		if q.Has("limit") && limit != p.Limit {
			return out, badPosition
		}
		limit = p.Limit
	} else if mode == "WATERMARK" {
		var err error
		p.Since, err = time.Parse(time.RFC3339Nano, q.Get("changedSince"))
		if err != nil {
			return out, badPosition
		}
		if c.RetainedFrom != nil && p.Since.Before(*c.RetainedFrom) {
			return out, expired
		}
		p.After = q.Get("afterId")
	} else if mode == "SNAPSHOT" && p.At.After(s.ClockFloor) {
		// Preserve the observation instant even if the host clock rolls back
		// across a restart between the snapshot and the next source change.
		if err := s.commit(transaction{}); err != nil {
			return out, unavailable
		}
	}
	p.Limit = limit
	rows := s.records(p)
	end := min(limit, len(rows))
	for _, e := range rows[:end] {
		out.Items = append(out.Items, render(s.Seed, e, sc))
	}
	out.HasMore = end < len(rows)
	tail := position{Collection: name, Mode: "CURSOR", Epoch: s.Epoch, At: p.At, Tail: true, Low: p.High, High: p.High, Poll: p.Poll + 1}
	if mode == "SNAPSHOT" {
		complete := !out.HasMore
		id := digest([]any{s.Epoch, name, p.High, p.At})
		out.SnapshotID, out.SnapshotComplete = &id, &complete
		out.SnapshotAt = &p.At
		if complete {
			out.Resume = &resume{SnapshotAt: p.At}
			if slices.Contains(c.Definition.Delta.Modes, "CURSOR") {
				out.Resume.Cursor = s.encode(tail)
			}
			if slices.Contains(c.Definition.Delta.Modes, "WATERMARK") {
				d, _ := duration(p.MaxLateness)
				out.Resume.Watermark = map[string]string{"changedSince": p.At.Add(-d).Format(time.RFC3339Nano)}
			}
		}
		if !complete || out.Resume.Cursor != "" || out.Resume.Watermark != nil {
			out.PositionExpiresAt = s.afterExpiry(name, p.High, !complete)
		}
	}
	if out.HasMore {
		last := keyOf(rows[end-1])
		p.Last = &last
		// Compaction preserves live snapshot objects in Base. Unread
		// tombstones and delta arrivals require a retained journal row.
		p.FirstUnread = 0
		for _, e := range rows[end:] {
			if mode == "SNAPSHOT" && e.Op != "DELETE" {
				continue
			}
			if p.FirstUnread == 0 || e.Seq < p.FirstUnread {
				p.FirstUnread = e.Seq
				t := e.RecordedAt.Add(s.cfg.Retention)
				out.PositionExpiresAt = &t
			}
		}
		next := s.encode(p)
		out.Next = &next
	} else if mode == "CURSOR" {
		next := s.encode(tail)
		out.Next = &next
		out.PositionExpiresAt = s.afterExpiry(name, p.High, false)
	}
	return out, nil
}
