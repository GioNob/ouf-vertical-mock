package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type object = map[string]any

func (s *store) catalogVersion() string {
	pairs := make([][2]string, 0, len(s.Collections))
	for _, name := range slices.Sorted(maps.Keys(s.Collections)) {
		pairs = append(pairs, [2]string{name, s.Collections[name].Active})
	}
	return digest(pairs)
}

func (s *store) version(name string) string {
	if c := s.Collections[name]; c != nil {
		return c.Active
	}
	return s.catalogVersion()
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
		status := http.StatusNoContent
		if r.Method != "GET" {
			status = http.StatusMethodNotAllowed
		} else if r.URL.Path == "/health/ready" {
			s.Lock()
			f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0600)
			if err == nil {
				err = f.Close()
			}
			if s.failed != nil || err != nil {
				status = http.StatusServiceUnavailable
			}
			s.Unlock()
		}
		w.WriteHeader(status)
		return
	}
	path := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	admin := path[0] == "admin"
	want := s.cfg.ReadToken
	if admin {
		want = s.cfg.AdminToken
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	hash := sha256.Sum256([]byte(value))
	if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(hash[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"code":"UNAUTHENTICATED"}`)
		return
	}

	body := http.MaxBytesReader(w, r.Body, 32<<10)
	defer body.Close()
	raw, err := io.ReadAll(body)
	name := ""
	if admin {
		name = r.URL.Query().Get("collection")
		if r.Method == "POST" {
			var scope struct{ Collection string }
			_ = json.Unmarshal(raw, &scope)
			name = scope.Collection
		}
	} else if path[0] == "collections" && len(path) >= 2 {
		name = path[1]
	}

	s.Lock()
	s.at = s.clock().UTC()
	var data any
	var prob *problem
	switch {
	case s.maintain() != nil:
		prob = unavailable
	case err != nil:
		prob = &problem{400, "INVALID_BODY"}
	case name != "" && s.Collections[name] == nil:
		prob = unknown
	case admin:
		data, prob = s.admin(r, raw, name)
	case path[0] == "collections":
		data, prob = s.read(w, r, path, name)
	default:
		prob = notFound
	}
	version := s.version(name)
	status := 200
	if prob != nil {
		status = prob.status
		msg := object{"code": prob.code, "message": http.StatusText(status), "schemaVersion": version}
		switch prob {
		case expired:
			msg["reason"] = "RETENTION"
		case oldEpoch:
			msg["reason"], msg["epoch"] = "EPOCH_CHANGED", s.Epoch
		}
		if s.Collections[name] != nil {
			msg["collection"] = name
		}
		data = msg
	}
	encoded, encodeErr := json.Marshal(data)
	s.at = time.Time{}
	s.Unlock()
	w.Header().Set("X-Schema-Version", version)
	if encodeErr != nil {
		status = 500
		encoded, _ = json.Marshal(object{"code": "ENCODING_ERROR", "message": "Internal Server Error", "schemaVersion": version})
	}
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func seconds(d time.Duration) int64 {
	return 1 + int64((d-1)/time.Second)
}

func (s *store) read(w http.ResponseWriter, r *http.Request, path []string, name string) (any, *problem) {
	if r.Method != "GET" {
		return nil, badMethod
	}
	now := s.now()
	for _, scope := range []string{"", name} {
		o := s.Outages[scope]
		if now.Before(o.Until) {
			retry := o.RetryAfter
			if retry == 0 && o.Kind == "RATE_LIMITED" {
				retry = seconds(o.Until.Sub(now))
			}
			if retry > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
			}
			if o.Kind == "RATE_LIMITED" {
				return nil, &problem{429, "RATE_LIMITED"}
			}
			return nil, unavailable
		}
	}
	if len(path) == 1 {
		var rows []object
		for _, name := range slices.Sorted(maps.Keys(s.Collections)) {
			c := s.Collections[name]
			d := c.Definition
			rows = append(rows, object{"collection": name, "epoch": s.Epoch, "title": d.Title, "schemaVersion": c.Active, "fieldsSha256": digest(c.Versions[c.Active].Fields),
				"deltaModes": d.Delta.Modes, "defaultMode": d.Delta.Default, "deletions": d.Delta.Deletions, "maxLateness": c.Rate.MaxLateness,
				"cycle": iso(s.Cycle), "retention": iso(s.cfg.Retention), "retainedFrom": c.RetainedFrom})
		}
		return object{"epoch": s.Epoch, "schemaVersion": s.catalogVersion(), "collections": rows}, nil
	}
	if len(path) != 3 || s.Collections[name] == nil {
		return nil, notFound
	}
	c := s.Collections[name]
	if path[2] == "schema" || path[2] == "items" {
		if wait := s.lastRead[name].Add(s.cfg.ReadInterval).Sub(now); wait > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(seconds(wait), 10))
			return nil, &problem{429, "RATE_LIMITED"}
		}
	}
	switch path[2] {
	case "schema":
		s.lastRead[name] = now
		sc := c.Versions[c.Active]
		return object{"collection": name, "schemaVersion": c.Active, "fields": sc.Fields, "key": sc.Key,
			"geometry": sc.Geometry, "fieldsSha256": digest(sc.Fields)}, nil
	case "items":
		data, prob := s.items(name, r.URL.Query())
		if prob == nil {
			s.lastRead[name] = now
		}
		return data, prob
	default:
		return nil, notFound
	}
}
