package main

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
)

func (s *store) row(e record, version string) object {
	c := s.Collections[e.Collection]
	it := render(s.Seed, e, c.Versions[version])
	var lateness, visibility any
	if e.Lateness != "" {
		lateness = e.Lateness
	}
	if e.Op == "DELETE" {
		visibility = "EXPLICIT"
		if !c.offersEvent(e) {
			visibility = "SNAPSHOT_ONLY"
		}
	}
	return object{"sequence": e.Seq, "epoch": s.Epoch, "recordedAt": e.RecordedAt, "collection": e.Collection,
		"origin": e.Origin, "kind": e.Kind, "lateness": lateness, "deleteVisibility": visibility,
		"emittedSchemaVersion": e.Version, "renderedSchemaVersion": version, "item": it, "contentSha256": digest(it)}
}

func (s *store) ledger(q url.Values, name string, state bool) (any, *problem) {
	limit, prob := pageLimit(q, 5000)
	if prob != nil {
		return nil, prob
	}
	version := q.Get("renderAs")
	if q.Has("renderAs") {
		known := false
		for n, c := range s.Collections {
			if _, ok := c.Versions[version]; ok && (name == "" || n == name) {
				known = true
				break
			}
		}
		if !known {
			return nil, &problem{400, "UNKNOWN_SCHEMA_VERSION"}
		}
	}
	if state {
		c := s.Collections[name]
		if c == nil {
			return nil, badPosition
		}
		if version == "" {
			version = c.Active
		}
		asOf := s.head()
		if q.Has("asOf") {
			var err error
			asOf, err = strconv.ParseInt(q.Get("asOf"), 10, 64)
			if len(q["asOf"]) != 1 || err != nil || asOf < 0 || asOf > s.head() {
				return nil, badPosition
			}
			if asOf < s.Floor {
				return nil, expired
			}
		}
		index := s.stateAt(name, asOf)
		items := []object{}
		after := q.Get("after")
		var next any
		for _, id := range slices.Sorted(maps.Keys(index)) {
			e := index[id]
			if id <= after || (e.Op == "DELETE" && e.Seq <= s.Floor) {
				continue
			}
			if len(items) == limit {
				next = items[len(items)-1]["id"]
				break
			}
			row := object{"id": id, "revision": e.Revision}
			if e.Op == "DELETE" {
				row["deletedAt"] = e.ChangedAt
			} else {
				it := render(s.Seed, e, c.Versions[version])
				row["changedAt"], row["properties"], row["contentSha256"] = it.ChangedAt, it.Properties, digest(it)
			}
			items = append(items, row)
		}
		return object{"collection": name, "asOf": asOf, "epoch": s.Epoch, "schemaVersion": c.Active, "renderedSchemaVersion": version, "items": items, "nextAfter": next, "hasMore": next != nil}, nil
	}
	since := s.Floor
	if q.Has("since") {
		var err error
		since, err = strconv.ParseInt(q.Get("since"), 10, 64)
		if err != nil || since < 0 || since > s.head() {
			return nil, badPosition
		}
		floor := s.Floor
		if name != "" {
			floor = s.Collections[name].Removed
		}
		if since < floor {
			return nil, expired
		}
	}
	rows := []object{}
	next, more := s.head(), false
	for _, e := range s.history[max(since-s.Floor, 0):] {
		if name != "" && name != e.Collection {
			continue
		}
		if len(rows) == limit {
			next, more = rows[len(rows)-1]["sequence"].(int64), true
			break
		}
		v := version
		if v == "" {
			v = e.Version
		} else if _, ok := s.Collections[e.Collection].Versions[v]; !ok {
			return nil, &problem{400, "UNKNOWN_SCHEMA_VERSION"}
		}
		rows = append(rows, s.row(e, v))
	}
	return object{"schemaVersion": s.version(name), "rows": rows, "nextSince": next, "hasMore": more}, nil
}
