package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type object = map[string]any

func (s *store) catalogVersion() string {
	versions := map[string]string{}
	for name, c := range s.Collections {
		versions[name] = c.Active
	}
	return digest(versions)
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 32<<10)
	defer body.Close()
	// Read before taking the journal lock, so a slow request cannot stall ticks.
	raw, err := io.ReadAll(body)
	s.Lock()
	version := s.catalogVersion()
	path := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(path) >= 2 && path[0] == "collections" && s.Collections[path[1]] != nil {
		version = s.Collections[path[1]].Active
	}
	status := 200
	var data any
	var prob *problem
	health := r.URL.Path == "/health/live" || r.URL.Path == "/health/ready"
	if err != nil {
		prob = &problem{400, "INVALID_BODY"}
	} else if health && r.Method == "GET" {
		if r.URL.Path == "/health/ready" && (s.failed != nil || s.now().Before(s.OutageUntil)) {
			status = 503
		}
	} else if path[0] == "admin" || path[0] == "collections" {
		want := s.cfg.ReadToken
		if path[0] == "admin" {
			want = s.cfg.AdminToken
		}
		scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		hash := sha256.Sum256([]byte(value))
		switch {
		case !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(hash[:], want[:]) != 1:
			w.Header().Set("WWW-Authenticate", "Bearer")
			prob = &problem{401, "UNAUTHORIZED"}
		case s.compact() != nil:
			prob = unavailable
		case path[0] == "admin":
			data, version, prob = s.admin(r, path, raw)
		case s.now().Before(s.OutageUntil):
			prob = unavailable
		default:
			data, prob = s.read(w, r, path, version)
		}
	} else {
		prob = &problem{404, "NOT_FOUND"}
	}
	if prob != nil {
		status = prob.status
		data = object{"schemaVersion": version, "code": prob.code}
	}
	// Marshal inside the lock, including schema maps shared with journal state.
	var encoded []byte
	if data != nil {
		encoded, err = json.Marshal(data)
		if err != nil {
			status = 500
			encoded, _ = json.Marshal(object{"schemaVersion": version, "code": "ENCODING_ERROR"})
		}
	}
	s.Unlock()
	w.Header().Set("X-Schema-Version", version)
	w.Header().Set("Cache-Control", "no-store")
	if len(encoded) > 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	if len(encoded) > 0 {
		_, _ = w.Write(encoded)
	}
}

func (s *store) read(w http.ResponseWriter, r *http.Request, path []string, version string) (any, *problem) {
	if r.Method != "GET" {
		return nil, &problem{405, "METHOD_NOT_ALLOWED"}
	}
	if len(path) == 1 {
		collections := []object{}
		for _, name := range slices.Sorted(maps.Keys(s.Collections)) {
			collections = append(collections, object{"collection": name, "title": map[string]string{"edicole": "Edicole comunali", "cantieri": "Cantieri comunali"}[name],
				"schemaVersion": s.Collections[name].Active, "deltaModes": []string{"CURSOR", "WATERMARK", "SNAPSHOT"}, "retention": s.cfg.RetentionText})
		}
		return object{"schemaVersion": version, "collections": collections}, nil
	}
	if len(path) != 3 || s.Collections[path[1]] == nil {
		return nil, &problem{404, "NOT_FOUND"}
	}
	name := path[1]
	switch path[2] {
	case "schema":
		sc := s.Collections[name].Versions[version]
		return object{"collection": name, "schemaVersion": version, "fields": sc.Fields, "key": sc.Key,
			"geometry": sc.Geometry, "fieldsFingerprint": digest(sc.Fields)}, nil
	case "items":
		now := s.now()
		if wait := s.lastRead[name].Add(s.Rate.ReadDelay).Sub(now); wait > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(1+int64((wait-1)/time.Second), 10))
			return nil, &problem{429, "RATE_LIMITED"}
		}
		data, prob := s.items(name, r.URL.Query())
		if prob == nil {
			s.lastRead[name] = now
		}
		return data, prob
	default:
		return nil, &problem{404, "NOT_FOUND"}
	}
}
