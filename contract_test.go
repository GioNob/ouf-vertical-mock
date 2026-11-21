package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const itemsURL = "/collections/edicole/items"

type lab struct {
	*testing.T
	*store
	clock time.Time
}

func newLab(t *testing.T) *lab {
	t.Helper()
	l := &lab{T: t, clock: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	c := config{Dir: t.TempDir(), Retention: 7*24*time.Hour, RetentionText: "P7D", Collections: []string{"edicole", "cantieri"},
		Seed: 73, Rate: rate{Count: 3, Interval: time.Minute, Create: 100}, ReadToken: sha256.Sum256([]byte("reader")), AdminToken: sha256.Sum256([]byte("admin"))}
	var err error
	l.store, err = openStore(c, func() time.Time { return l.clock })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *lab) tick() {
	l.Helper()
	if err := l.cycle(); err != nil {
		l.Fatal(err)
	}
}

func (l *lab) request(method, path, body, token string, status int) *httptest.ResponseRecorder {
	l.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != status || w.Header().Get("X-Schema-Version") == "" {
		l.Fatalf("%s %s: status %d, body %s", method, path, w.Code, w.Body.String())
	}
	if w.Body.Len() > 0 {
		var v struct {
			Version string `json:"schemaVersion"`
		}
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Version != w.Header().Get("X-Schema-Version") {
			l.Fatalf("schemaVersion missing or inconsistent in %s", w.Body.String())
		}
	}
	return w
}

func (l *lab) readPage(path string) envelope {
	l.Helper()
	w := l.request("GET", path, "", "reader", 200)
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		l.Fatal(err)
	}
	return e
}

type receipt struct {
	Version  string `json:"schemaVersion"`
	Expected []struct {
		Sequence int64 `json:"sequence"`
		Item item `json:"item"`
	} `json:"expected"`
	NextSince int64 `json:"nextSince"`
}

func (l *lab) command(path, body string) receipt {
	l.Helper()
	w := l.request("POST", "/admin/"+path, body, "admin", 200)
	var result receipt
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Expected == nil {
		l.Fatalf("invalid receipt: %s", w.Body.String())
	}
	return result
}

func (l *lab) restart() {
	l.Helper()
	reopened, err := openStore(l.cfg, func() time.Time { return l.clock })
	if err != nil {
		l.Fatal(err)
	}
	l.store = reopened
}

func (l *lab) collect(path, mode string, limit int) ([]item, envelope) {
	l.Helper()
	p := l.readPage(path)
	var all []item
	for pages := 0; ; pages++ {
		if pages > 50 || p.Mode != mode || len(p.Items) > limit || p.Items == nil {
			l.Fatalf("invalid page: %+v", p)
		}
		if mode == "SNAPSHOT" {
			if p.SnapshotComplete == nil || *p.SnapshotComplete == p.HasMore {
				l.Fatal("invalid snapshotComplete")
			}
		} else if p.SnapshotComplete != nil {
			l.Fatal("unexpected snapshotComplete")
		}
		all = append(all, p.Items...)
		if !p.HasMore {
			return all, p
		}
		if p.Next == nil {
			l.Fatal("missing next")
		}
		key := "cursor"
		if mode == "SNAPSHOT" {
			key = "page"
		}
		p = l.readPage(path+"&"+key+"="+url.QueryEscape(*p.Next))
	}
}

func TestModesOrderingLateDuplicatesAndDelete(t *testing.T) {
	l := newLab(t)
	l.tick()
	initial := l.readPage(itemsURL)
	target := initial.Items[0]
	l.clock = l.clock.Add(time.Minute)
	dup := l.command("inject", `{"collection":"edicole","kind":"duplicate","id":"`+target.ID+`"}`)
	if !reflect.DeepEqual(dup.Expected[0].Item, target) {
		t.Fatal("duplicate changed data")
	}
	late := l.command("inject", `{"collection":"edicole","kind":"late","minutes":5,"id":"`+target.ID+`"}`).Expected[0].Item
	if late.Revision != target.Revision+1 || !late.ChangedAt.Before(l.clock) {
		t.Fatal("late revision or timestamp")
	}
	deleted := l.command("inject", `{"collection":"edicole","kind":"delete","id":"`+target.ID+`"}`).Expected[0].Item
	if deleted.Op != "DELETE" || deleted.Properties != nil || deleted.Revision != late.Revision+1 {
		t.Fatal("invalid tombstone")
	}
	events, tail := l.collect(itemsURL+"?limit=1", "CURSOR", 1)
	water, _ := l.collect(itemsURL+"?mode=WATERMARK&limit=1", "WATERMARK", 1)
	if len(events) != 6 || !reflect.DeepEqual(water, events) || tail.Next == nil {
		t.Fatal("lost delta or checkpoint")
	}
	for i := 1; i < len(events); i++ {
		prev, next := events[i-1], events[i]
		if prev.ChangedAt.After(next.ChangedAt) || (prev.ChangedAt.Equal(next.ChangedAt) &&
			(prev.ID > next.ID || (prev.ID == next.ID && prev.Revision > next.Revision))) {
			t.Fatal("unstable delta order")
		}
	}
	snap, last := l.collect(itemsURL+"?mode=SNAPSHOT&limit=1", "SNAPSHOT", 1)
	if len(snap) != 2 || last.Next != nil || slices.ContainsFunc(snap, func(i item) bool { return i.ID == target.ID }) {
		t.Fatal("deleted item still present")
	}
	remain := l.readPage(itemsURL+"?mode=WATERMARK&changedSince="+url.QueryEscape(target.ChangedAt.Format(time.RFC3339Nano))+"&afterId="+target.ID)
	if len(remain.Items) != 3 || remain.Items[0].ID <= target.ID {
		t.Fatal("watermark tie breaker")
	}
	w := l.request("GET", "/admin/expected?since=6", "", "admin", 200)
	var ledger receipt
	if err := json.Unmarshal(w.Body.Bytes(), &ledger); err != nil || ledger.NextSince != 9 || len(ledger.Expected) != 3 ||
		ledger.Expected[2].Sequence != 9 || !reflect.DeepEqual(ledger.Expected[2].Item, deleted) {
		t.Fatal("expected log disagrees with the API")
	}
}

func TestFrozenSnapshotAndCursorReplayAfterRestart(t *testing.T) {
	l := newLab(t)
	l.tick()
	p := l.readPage(itemsURL+"?mode=SNAPSHOT&limit=1")
	current := l.readPage(itemsURL)
	lastID := current.Items[2].ID
	l.command("inject", `{"collection":"edicole","kind":"delete","id":"`+lastID+`"}`)
	remaining := l.readPage(itemsURL+"?mode=SNAPSHOT&page="+*p.Next)
	if len(remaining.Items) != 2 || remaining.Items[1].ID != lastID || !*remaining.SnapshotComplete {
		t.Fatal("snapshot moved")
	}
	poll := itemsURL+"?cursor="+*current.Next
	before := l.readPage(poll)
	l.clock = l.clock.Add(time.Minute)
	l.command("inject", `{"collection":"edicole","kind":"late","minutes":4}`)
	l.restart()
	replay := l.readPage(poll)
	if !reflect.DeepEqual(before, replay) || len(replay.Items) != 1 || replay.Items[0].ID != lastID {
		t.Fatal("replay changed")
	}
	late := l.readPage(itemsURL+"?cursor="+*before.Next)
	if len(late.Items) != 1 || !late.Items[0].ChangedAt.Before(l.clock) {
		t.Fatal("cursor missed arrival behind the temporal watermark")
	}
}

type descriptor struct {
	Version     string  `json:"schemaVersion"`
	Fields      []field `json:"fields"`
	Fingerprint string  `json:"fieldsFingerprint"`
}

func TestSchemaEvolutionRevertAndProjection(t *testing.T) {
	l := newLab(t)
	l.tick()
	describe := func() descriptor {
		var d descriptor
		w := l.request("GET", "/collections/edicole/schema", "", "reader", 200)
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || digest(d.Fields) != d.Fingerprint {
			t.Fatalf("invalid descriptor: %s", w.Body.String())
		}
		return d
	}
	original := describe()
	last := original
	for _, edit := range []string{
		`"action":"add","name":"posti","type":"integer","description":"Posti disponibili"`,
		`"action":"rename","name":"posti","newName":"capienza"`,
		`"action":"type","name":"capienza","type":"boolean"`,
		`"action":"remove","name":"telefono"`,
	} {
		result := l.command("schema", `{"collection":"edicole",`+edit+`}`)
		d := describe()
		if d.Version == last.Version || d.Fingerprint == last.Fingerprint || result.Version != d.Version {
			t.Fatal("schema or fingerprint did not change")
		}
		l.tick()
		p := l.readPage(itemsURL)
		for _, it := range p.Items {
			if p.Version != d.Version || len(it.Properties) != len(d.Fields) {
				t.Fatal("incoherent schema envelope")
			}
			for _, f := range d.Fields {
				v := it.Properties[f.Name]
				if v == nil || (f.Type == "boolean" && reflect.TypeOf(v).Kind() != reflect.Bool) ||
					(f.Type == "integer" && reflect.TypeOf(v).Kind() != reflect.Float64) {
					t.Fatalf("wrong property %s: %v", f.Name, v)
				}
			}
		}
		last = d
	}
	l.restart()
	l.command("schema", `{"collection":"edicole","action":"revert","version":"`+original.Version+`"}`)
	if !reflect.DeepEqual(describe(), original) || l.readPage(itemsURL).Version != original.Version {
		t.Fatal("revert failed")
	}
	if digest([]field{{"Caffè", "id", false, "string"}}) != "9d6a480a969b79b83dc6398f197b32c3c9dffa10e8cdd82dd34fa250fa632610" {
		t.Fatal("canonical UTF-8 fingerprint")
	}
}

func TestGeneratedCollectionsAndSeed(t *testing.T) {
	a, b := newLab(t), newLab(t)
	for _, l := range []*lab{a, b} {
		l.tick()
		l.command("rate", `{"create":0,"update":100,"delete":0}`)
		l.tick()
		l.command("rate", `{"create":0,"update":0,"delete":100}`)
		l.tick()
	}
	for _, name := range []string{"cantieri", "edicole"} {
		path := "/collections/"+name+"/items"
		first, second := a.readPage(path), b.readPage(path)
		if !reflect.DeepEqual(first.Items, second.Items) || len(first.Items) != 9 {
			t.Fatal("seed or operation count")
		}
		if len(a.readPage(path+"?mode=SNAPSHOT").Items) != 0 {
			t.Fatal("delete rate did not empty collection")
		}
		for _, it := range first.Items {
			if it.Op == "DELETE" {
				continue
			}
			props := it.Properties
			if props["id"] != it.ID {
				t.Fatal("natural key")
			}
			g := props["geometria"].(map[string]any)
			if name == "cantieri" {
				ring := g["coordinates"].([]any)[0].([]any)
				if g["type"] != "Polygon" || len(ring) < 4 || !reflect.DeepEqual(ring[0], ring[len(ring)-1]) ||
					props["data_inizio"].(string) >= props["data_fine"].(string) {
					t.Fatal("invalid construction site")
				}
			} else if g["type"] != "Point" || len(g["coordinates"].([]any)) != 2 || len(props) != 9 {
				t.Fatal("invalid newsstand")
			}
		}
	}
}

func TestConfigAndScheduler(t *testing.T) {
	dir := t.TempDir()
	// Set every configurable option to isolate the test from operator settings.
	for k, v := range map[string]string{
		"PORT": "8080", "DATA_DIR": dir, "RETENTION": "P1DT2H3M4S", "SEED": "12",
		"COLLECTIONS": "cantieri", "COUNT": "1", "INTERVAL": "5ms",
		"CREATE": "100", "UPDATE": "0", "DELETE": "0", "READ_DELAY": "0s",
	} {
		t.Setenv("OUF_"+k, v)
	}
	for _, name := range []string{"READ_TOKEN_FILE", "ADMIN_TOKEN_FILE"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("test-"+name+"\r\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OUF_"+name, path)
	}
	c, err := loadConfig()
	if err != nil || c.Retention != 26*time.Hour+3*time.Minute+4*time.Second {
		t.Fatalf("config: %+v", err)
	}
	s, err := openStore(c, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.schedule(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(3*time.Second)
	for {
		s.Lock()
		head := s.head()
		s.Unlock()
		if head >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not generate two cycles")
		}
		time.Sleep(5*time.Millisecond)
	}
	t.Setenv("OUF_READ_TOKEN_FILE", "")
	t.Setenv("OUF_READ_TOKEN", "plain-text-is-ignored")
	if _, err = loadConfig(); err == nil {
		t.Fatal("plaintext credential accepted")
	}
}

func TestRetentionCompactionAndTornTail(t *testing.T) {
	l := newLab(t)
	l.tick()
	old := l.readPage(itemsURL+"?limit=1")
	l.clock = l.clock.Add(l.cfg.Retention + time.Second)
	l.tick()
	for _, path := range []string{itemsURL+"?cursor="+*old.Next, itemsURL+"?mode=WATERMARK&changedSince="+url.QueryEscape(old.Items[0].ChangedAt.Format(time.RFC3339Nano))} {
		w := l.request("GET", path, "", "reader", 410)
		if !strings.Contains(w.Body.String(), "CURSOR_EXPIRED") {
			t.Fatal("wrong error")
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"events":[`); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	l.restart()
	if len(l.readPage(itemsURL).Items) != 3 || len(l.readPage(itemsURL+"?mode=SNAPSHOT").Items) != 6 || l.Floor != 6 {
		t.Fatal("incorrect compacted state")
	}
	l.tick()
	l.restart()
	if l.head() != 18 {
		t.Fatal("failed to append after recovery")
	}
}

func TestAuthHealthThrottleOutageAndReset(t *testing.T) {
	l := newLab(t)
	for _, token := range []string{"", "reader"} {
		l.request("GET", "/admin/expected", "", token, 401)
	}
	for _, token := range []string{"", "admin"} {
		l.request("GET", "/collections", "", token, 401)
	}
	l.request("GET", "/collections", "", "reader", 200)
	l.command("rate", `{"readDelay":"1500ms","count":1}`)
	l.tick()
	p := l.readPage(itemsURL)
	w := l.request("GET", itemsURL, "", "reader", 429)
	if w.Header().Get("Retry-After") != "2" {
		t.Fatal("Retry-After")
	}
	l.command("outage", `{"duration":"4s"}`)
	l.restart()
	l.request("GET", "/collections", "", "reader", 503)
	for _, h := range []struct {
		path string
		code int
	}{{"live", 200}, {"ready", 503}} {
		if l.request("GET", "/health/"+h.path, "", "", h.code).Body.Len() != 0 {
			t.Fatal("probe disclosed data")
		}
	}
	l.clock = l.clock.Add(5*time.Second)
	l.request("GET", "/health/ready", "", "", 200)
	l.request("GET", itemsURL, "", "reader", 200)
	l.command("reset", "{}")
	l.request("GET", itemsURL+"?cursor="+*p.Next, "", "reader", 410)
	if len(l.readPage(itemsURL).Items) != 0 || l.Rate.Count != 3 {
		t.Fatal("reset")
	}
}
