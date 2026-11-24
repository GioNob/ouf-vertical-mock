package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type item struct {
	ID         string         `json:"id"`
	Revision   int64          `json:"revision"`
	Op         string         `json:"op"`
	ChangedAt  time.Time      `json:"changedAt"`
	Properties map[string]any `json:"properties,omitempty"`
}

func sample(seed int64, collection, id, origin string, rev int64, draw int) uint64 {
	b := fmt.Appendf(nil, "%d:%q:%q:%q:%d:%d", seed, collection, id, origin, rev, draw)
	sum := sha256.Sum256(b)
	return binary.BigEndian.Uint64(sum[:8])
}

func render(seed int64, r record, s schema) item {
	it := item{ID: r.ID, Revision: r.Revision, Op: r.Op, ChangedAt: r.ChangedAt}
	if it.Op == "DELETE" {
		return it
	}
	it.Properties = make(map[string]any, len(s.Fields))
	// Cache underlying values separately so relative dates can use their
	// anchor even when the anchor's nullable projection is null.
	values := map[string]any{}
	var value func(string) any
	value = func(name string) any {
		if v, ok := values[name]; ok {
			return v
		}
		g := s.Generators[name]
		var rev int64
		if g.Varies == "REVISION" {
			rev = r.Revision
		}
		i := 0
		draw := func() uint64 {
			i++
			return sample(seed, r.Collection, r.ID, s.Origins[name], rev, i)
		}
		between := func(lo, hi int64) int64 {
			return lo + int64(draw()%uint64(hi-lo+1))
		}
		var v any
		switch g.Kind {
		case "id":
			v = r.ID
		case "choice":
			v = g.Values[draw()%uint64(len(g.Values))]
		case "template":
			v = placeholder.ReplaceAllStringFunc(g.Pattern, func(token string) string {
				if strings.HasPrefix(token, "{choice:") {
					opts := strings.Split(token[8:len(token)-1], "|")
					return opts[draw()%uint64(len(opts))]
				}
				a, b, _ := strings.Cut(token[5:len(token)-1], "..")
				lo, _ := strconv.ParseInt(a, 10, 64)
				hi, _ := strconv.ParseInt(b, 10, 64)
				return strconv.FormatInt(between(lo, hi), 10)
			})
		case "integer", "decimal":
			lo, hi, factor := g.numericBounds()
			v = float64(between(lo, hi)) / factor
		case "boolean":
			v = draw()%2 == 1
		case "date":
			if g.After != "" {
				base, _ := time.Parse(time.DateOnly, value(g.After).(string))
				v = base.AddDate(0, 0, int(between(int64(*g.MinDays), int64(*g.MaxDays)))).Format(time.DateOnly)
			} else {
				a, _ := time.Parse(time.DateOnly, g.From)
				b, _ := time.Parse(time.DateOnly, g.To)
				v = a.AddDate(0, 0, int(between(0, (b.Unix()-a.Unix())/86400))).Format(time.DateOnly)
			}
		case "datetime":
			if g.Source == "CHANGED_AT" {
				v = r.ChangedAt
			} else {
				a, _ := time.Parse(time.RFC3339Nano, g.From)
				b, _ := time.Parse(time.RFC3339Nano, g.To)
				sec := between(a.Unix(), b.Unix())
				lo, hi := int64(0), int64(time.Second-1)
				if sec == a.Unix() {
					lo = int64(a.Nanosecond())
				}
				if sec == b.Unix() {
					hi = int64(b.Nanosecond())
				}
				v = time.Unix(sec, between(lo, hi)).UTC()
			}
		case "point", "polygon":
			b := s.BBox
			dx, dy := b[2]-b[0], b[3]-b[1]
			x := b[0] + dx*(float64(draw()%9000)/10000)
			y := b[1] + dy*(float64(draw()%9000)/10000)
			if g.Kind == "point" {
				v = object{"type": "Point", "coordinates": []float64{x, y}}
			} else {
				xx, yy := x+dx/20, y+dy/20
				if xx <= x || xx > b[2] {
					x, xx = b[0], b[2]
				}
				if yy <= y || yy > b[3] {
					y, yy = b[1], b[3]
				}
				ring := [][]float64{{x, y}, {xx, y}, {xx, yy}, {x, yy}, {x, y}}
				v = object{"type": "Polygon", "coordinates": [][][]float64{ring}}
			}
		}
		values[name] = v
		projected := v
		if g.NullPercent != nil && int(draw()%100) < *g.NullPercent {
			projected = nil
		}
		it.Properties[name] = projected
		return v
	}
	for _, f := range s.Fields {
		value(f.Name)
	}
	return it
}
