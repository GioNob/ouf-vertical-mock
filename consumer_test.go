package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Test-only model of a single leased partition. One atomic file replacement
// represents the transaction containing durable outcomes and the checkpoint.
type lastState struct {
	Revision, Basis int64
	Retired         bool
	Hash, Bundle    string
	UUID            string
	Properties      object
}

type observation struct {
	ID, Hash, Outcome string
	Revision, Basis   int64
}

type consumer struct {
	Confirmed, Version, Fields, Bundle, Block, Phase string
	Candidate                                        string
	PageIndex                                        int
	Generation                                       int64
	Objects                                          map[string]lastState
	Lineage                                          []observation
}

func (c *consumer) apply(it item, basis int64) string {
	prev, exists := c.Objects[it.ID]
	hash := digest(it)
	equal := it.Revision == prev.Revision && basis == prev.Basis
	retire := it.Op == "DELETE"
	outcome := "APPLIED"
	switch {
	case exists && (it.Revision < prev.Revision || (it.Revision == prev.Revision && basis < prev.Basis)):
		outcome = "SUPERSEDED" // R5
	case exists && equal:
		switch {
		case hash == prev.Hash:
			outcome = "DUPLICATE" // R2
		case c.Bundle != prev.Bundle:
			outcome = "DUPLICATE_REPUBLISHED" // R3
		default:
			outcome, c.Block = "CONFLICT", "EVENT_CONFLICT" // R4
		}
	default:
		switch {
		case retire && exists && prev.Retired:
			outcome = "DUPLICATE" // R9 still advances lastKey
		case retire:
			outcome = "RETIRED" // R7, R10
		case exists && prev.Retired:
			outcome = "REACTIVATED" // R8
		}
		uuid := prev.UUID
		if !retire && uuid == "" {
			sum := sha256.Sum256(canonical([]string{"test-tenant", "test-source", "edicole", it.ID}))
			sum[6] = sum[6]&0x0f | 0x80 // application-defined UUIDv8
			sum[8] = sum[8]&0x3f | 0x80
			uuid = fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
		}
		c.Objects[it.ID] = lastState{it.Revision, basis, retire, hash, c.Bundle, uuid, it.Properties}
	}
	c.Lineage = append(c.Lineage, observation{it.ID, hash, outcome, it.Revision, basis})
	return outcome
}

func (c *consumer) save(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(f).Encode(c); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func restore(t *testing.T, path string) consumer {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c consumer
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *consumer) snapshot(l *lab, path string, approvedAbsences bool) {
	l.Helper()
	q := url.Values{"mode": {"SNAPSHOT"}, "limit": {"3"}}
	seen := map[string]bool{}
	var items []item
	var id string
	for {
		p := l.page(itemsURL + "?" + q.Encode())
		if p.SnapshotID == nil || (id != "" && *p.SnapshotID != id) || p.FieldsSha256 != c.Fields || p.Version != c.Version {
			c.Block = "SCHEMA_CHANGED"
			c.save(l.T, path)
			return
		}
		id = *p.SnapshotID
		for _, it := range p.Items {
			seen[it.ID] = true
			items = append(items, it)
		}
		if p.HasMore {
			q.Set("page", *p.Next)
			continue
		}
		if !*p.SnapshotComplete || p.Resume == nil || p.Resume.Cursor == "" {
			l.Fatal("incomplete snapshot")
		}
		var absent []string
		active := 0
		for id, state := range c.Objects {
			if !state.Retired {
				active++
				if !seen[id] {
					absent = append(absent, id)
				}
			}
		}
		if len(absent) > 0 && !approvedAbsences && (c.Block == "POSITION_LOST" || len(absent)*100 > active*20) {
			c.Block = "MASS_DELETION_GUARD"
			c.save(l.T, path)
			return
		}
		c.Generation++
		for _, it := range items {
			c.apply(it, c.Generation)
		}
		for _, id := range absent {
			c.apply(item{ID: id, Revision: c.Objects[id].Revision, ChangedAt: p.Resume.SnapshotAt, Op: "DELETE"}, c.Generation)
		}
		c.Confirmed, c.Phase, c.Block = p.Resume.Cursor, "CATCH_UP", ""
		c.Candidate, c.PageIndex = "", 0
		c.save(l.T, path)
		return
	}
}

// Treat status and schema as observations before looking at an item. A page
// advances only in the transaction containing its last outcome.
func (c *consumer) poll(l *lab, path string) {
	l.Helper()
	c.pollUntil(l, path, 0)
}

func (c *consumer) pollUntil(l *lab, path string, interruptAfter int) {
	l.Helper()
	if c.Block != "" && c.Block != "SCHEMA_CHANGED" {
		return
	}
	for {
		r := httptest.NewRequest("GET", itemsURL+"?limit=3&cursor="+url.QueryEscape(c.Confirmed), nil)
		r.Header.Set("Authorization", "Bearer reader")
		w := httptest.NewRecorder()
		l.ServeHTTP(w, r)
		switch w.Code {
		case 429, 503:
			return
		case 410:
			failure := decode[struct{ Reason string }](l.T, w)
			c.Block = "POSITION_LOST"
			if failure.Reason == "EPOCH_CHANGED" {
				c.Block = "SOURCE_EPOCH_CHANGED"
			}
			c.save(l.T, path)
			return
		case 200:
		default:
			l.Fatalf("poll failed: %s", w.Body.String())
		}
		var header struct{ SchemaVersion, FieldsSha256 string }
		if err := json.Unmarshal(w.Body.Bytes(), &header); err != nil {
			l.Fatal(err)
		}
		if header.SchemaVersion != c.Version || header.FieldsSha256 != c.Fields {
			c.Block = "SCHEMA_CHANGED"
			c.save(l.T, path)
			return
		}
		c.Block = ""
		p := decode[envelope](l.T, w)
		if c.Candidate != "" && c.Candidate != *p.Next {
			l.Fatal("replayed page changed candidate")
		}
		c.Candidate = *p.Next
		for i := c.PageIndex; i < len(p.Items); i++ {
			if c.apply(p.Items[i], 0) == "CONFLICT" {
				c.save(l.T, path)
				return
			}
			c.PageIndex = i + 1
			if c.PageIndex < len(p.Items) {
				c.save(l.T, path)
				if interruptAfter == c.PageIndex {
					return
				}
			}
		}
		c.Confirmed, c.Candidate, c.PageIndex = c.Candidate, "", 0
		if !p.HasMore {
			c.Phase = "DELTA"
		}
		c.save(l.T, path)
		if !p.HasMore {
			return
		}
	}
}

type stateRow struct {
	ID, ContentSha256 string
	Revision          int64
	DeletedAt         *time.Time
	Properties        object
}

func checkInvariants(l *lab, c *consumer) {
	l.Helper()
	type key struct {
		id   string
		rev  int64
		hash string
	}
	// Only the test oracle inspects the mock's signed checkpoint. A tail
	// covers all arrivals through Low, even when the source has moved on.
	confirmed, prob := l.decode(c.Confirmed, "edicole", "CURSOR")
	if prob != nil || !confirmed.Tail {
		l.Fatal("I3 needs a confirmed, complete arrival window")
	}
	asOf := confirmed.Low
	want, got := map[key]int{}, map[key]int{}
	var since int64
	for since < asOf {
		p := l.expected("collection=edicole&renderAs=" + c.Version + "&limit=17&since=" + decimalSeq(since))
		for _, r := range p.Rows {
			if r.Sequence > asOf {
				break
			}
			if r.Sequence <= since || r.ContentSha256 != digest(r.Item) || r.RenderedSchemaVersion != c.Version {
				l.Fatalf("invalid oracle row %d", r.Sequence)
			}
			want[key{r.Item.ID, r.Item.Revision, r.ContentSha256}]++
			since = r.Sequence
		}
		if !p.HasMore || p.NextSince >= asOf {
			break
		}
	}
	effects := map[[2]any]bool{}
	for _, row := range c.Lineage {
		if row.Basis != 0 {
			continue
		}
		got[key{row.ID, row.Revision, row.Hash}]++
		if slices.Contains([]string{"APPLIED", "RETIRED", "REACTIVATED"}, row.Outcome) {
			k := [2]any{row.ID, row.Revision}
			if effects[k] {
				l.Fatalf("I2 duplicate effect for %v", k)
			}
			effects[k] = true
		}
	}
	if !maps.Equal(want, got) {
		l.Fatal("I1 ledger and durable outcome multiplicities differ")
	}
	count, after := 0, ""
	for {
		p := decode[struct {
			AsOf      *int64
			Epoch     string
			Items     []stateRow
			NextAfter *string
		}](l.T, l.request("GET", "/admin/expected/state?collection=edicole&asOf="+decimalSeq(asOf)+"&limit=3&renderAs="+c.Version+"&after="+url.QueryEscape(after), "", "admin", 200))
		if p.AsOf == nil || *p.AsOf != asOf || p.Epoch != confirmed.Epoch {
			l.Fatal("I3 oracle returned a different cut")
		}
		for _, r := range p.Items {
			state, ok := c.Objects[r.ID]
			if !ok || state.Revision != r.Revision || state.Retired != (r.DeletedAt != nil) ||
				(!state.Retired && (state.Hash != r.ContentSha256 || !reflect.DeepEqual(state.Properties, r.Properties))) {
				l.Fatalf("I3 state mismatch for %s at %d", r.ID, r.Revision)
			}
			count++
		}
		if p.NextAfter == nil {
			break
		}
		after = *p.NextAfter
	}
	if count != len(c.Objects) {
		l.Fatal("I3 extra objects")
	}
}

func TestReferenceConsumer(t *testing.T) {
	l := newLab(t)
	sc := l.page(itemsURL)
	c := consumer{Version: sc.Version, Fields: sc.FieldsSha256, Bundle: "approved", Objects: map[string]lastState{}, Phase: "FULL_SNAPSHOT"}
	path := filepath.Join(t.TempDir(), "consumer.json")
	c.snapshot(l, path, false)
	c.poll(l, path)
	rng := rand.New(rand.NewSource(731))
	l.inject("edicole", "create", "A")
	l.command("inject", `{"collection":"edicole","kind":"late","id":"A","lateness":"PT2S"}`)
	l.clock = l.clock.Add(time.Second)
	l.inject("edicole", "delete", "A")
	c.poll(l, path)
	l.clock = l.clock.Add(2 * time.Second)
	c.poll(l, path)
	if c.Lineage[len(c.Lineage)-1].Outcome != "SUPERSEDED" || !c.Objects["A"].Retired {
		t.Fatal("I5 late UPSERT resurrected a retired object")
	}
	for cycle := range 220 {
		l.clock = l.clock.Add(time.Second)
		id := fmt.Sprintf("object-%02d", rng.Intn(12))
		state, exists := c.Objects[id]
		switch {
		case !exists || state.Retired:
			l.inject("edicole", "create", id)
		default:
			switch rng.Intn(5) {
			case 0:
				l.inject("edicole", "duplicate", id)
			case 1:
				l.command("inject", `{"collection":"edicole","kind":"late","lateness":"PT3S","id":"`+id+`"}`)
			case 2:
				l.inject("edicole", "delete", id)
			default:
				l.inject("edicole", "update", id)
			}
		}
		if cycle%39 == 8 {
			l.command("schema", `{"collection":"edicole","action":"add","name":"extra","type":"integer"}`)
			pos, n := c.Confirmed, len(c.Lineage)
			c.poll(l, path)
			l.restart()
			c = restore(l.T, path)
			c.poll(l, path)
			if c.Block != "SCHEMA_CHANGED" || c.Confirmed != pos || len(c.Lineage) != n {
				t.Fatal("unapproved page advanced durable position")
			}
			l.command("schema", `{"collection":"edicole","action":"revert","version":"`+c.Version+`"}`)
		}
		if cycle%31 == 7 {
			k := []string{"UNAVAILABLE", "RATE_LIMITED"}[rng.Intn(2)]
			l.command("outage", `{"collection":"edicole","duration":"PT1S","kind":"`+k+`"}`)
			pos := c.Confirmed
			c.poll(l, path)
			if c.Confirmed != pos {
				t.Fatal("outage advanced checkpoint")
			}
			l.clock = l.clock.Add(time.Second)
		}
		c.poll(l, path)
		if cycle%47 == 13 {
			l.restart()
			c = restore(l.T, path)
		}
		if cycle%25 == 0 {
			checkInvariants(l, &c)
		}
	}
	l.command("clock", `{"advance":"PT10M"}`)
	c.poll(l, path)
	checkInvariants(l, &c)
}

func TestReferenceBootstrapWithRetainedDelete(t *testing.T) {
	l := newLab(t)
	l.inject("edicole", "create", "A")
	l.command("inject", `{"collection":"edicole","kind":"late","id":"A","lateness":"PT2S"}`)
	deletion := l.inject("edicole", "delete", "A").Rows[0]
	sc := l.page(itemsURL)
	c := consumer{Version: sc.Version, Fields: sc.FieldsSha256, Objects: map[string]lastState{}}
	path := filepath.Join(t.TempDir(), "consumer.json")
	c.snapshot(l, path, false)
	state := c.Objects["A"]
	if !state.Retired || state.UUID != "" || state.Revision != deletion.Item.Revision {
		t.Fatal("bootstrap lost the retained tombstone")
	}
	l.command("clock", `{"advance":"PT2S"}`)
	c.poll(l, path)
	if len(c.Lineage) != 2 || c.Lineage[1].Outcome != "SUPERSEDED" || c.Objects["A"].Revision != state.Revision || !c.Objects["A"].Retired {
		t.Fatal("catch-up resurrected an object deleted before bootstrap")
	}
	confirmed := c.Confirmed
	l.command("reset", "{}")
	l.inject("edicole", "create", "A")
	c.poll(l, path)
	c = restore(t, path)
	if c.Block != "SOURCE_EPOCH_CHANGED" || c.Confirmed != confirmed || len(c.Lineage) != 2 {
		t.Fatal("old position crossed into a new history")
	}
}

func TestReferenceRulesAndRecovery(t *testing.T) {
	c := consumer{Bundle: "first", Objects: map[string]lastState{}}
	it := item{ID: "A", Revision: 2, Op: "DELETE"}
	if c.apply(it, 0) != "RETIRED" || c.Objects["A"].UUID != "" || c.apply(it, 0) != "DUPLICATE" {
		t.Fatal("R10/R2")
	}
	it.Revision++
	if c.apply(it, 0) != "DUPLICATE" || c.Objects["A"].Revision != 3 {
		t.Fatal("R9")
	}
	it.Op, it.Revision = "UPSERT", 4
	if c.apply(it, 0) != "REACTIVATED" || c.Objects["A"].UUID == "" {
		t.Fatal("R8")
	}
	it.Properties = object{"approved": true}
	c.Bundle = "second"
	if c.apply(it, 0) != "DUPLICATE_REPUBLISHED" {
		t.Fatal("R3")
	}
	c.Bundle = "first"
	if c.apply(it, 0) != "CONFLICT" || c.Block != "EVENT_CONFLICT" {
		t.Fatal("R4")
	}

	l := newLab(t)
	l.inject("edicole", "create", "A")
	page := l.page(itemsURL)
	c = consumer{Version: page.Version, Fields: page.FieldsSha256, Objects: map[string]lastState{}}
	path := filepath.Join(t.TempDir(), "recovery.json")
	c.snapshot(l, path, false)
	pos := c.Confirmed
	l.inject("edicole", "delete", "A")
	l.command("clock", `{"advance":"P8D"}`)
	c.poll(l, path)
	if c.Block != "POSITION_LOST" || c.Confirmed != pos {
		t.Fatal("expired position reset silently")
	}
	c.snapshot(l, path, false)
	if c.Block != "MASS_DELETION_GUARD" || c.Confirmed != pos || c.Objects["A"].Retired {
		t.Fatal("unguarded absence")
	}
	c.snapshot(l, path, true)
	if !c.Objects["A"].Retired || c.Generation != 2 || c.Objects["A"].Basis != 2 {
		t.Fatal("approved absence not applied")
	}
}

func TestReferenceRestartInsidePage(t *testing.T) {
	l := newLab(t)
	sc := l.page(itemsURL)
	c := consumer{Version: sc.Version, Fields: sc.FieldsSha256, Bundle: "approved", Objects: map[string]lastState{}}
	path := filepath.Join(t.TempDir(), "consumer.json")
	c.snapshot(l, path, false)
	start := c.Confirmed
	l.command("inject", `{"collection":"edicole","kind":"create","count":3}`)
	c.pollUntil(l, path, 1)
	c = restore(t, path)
	if c.Confirmed != start || c.Candidate == "" || c.PageIndex != 1 || len(c.Lineage) != 1 {
		t.Fatal("in-flight page advanced the confirmed checkpoint")
	}
	l.restart()
	l.inject("edicole", "create", "after-restart")
	c.poll(l, path)
	if len(c.Lineage) != 3 {
		t.Fatal("restart duplicated the first outcome or changed the page")
	}
	checkInvariants(l, &c) // the fourth arrival is still beyond the confirmed window
	c.poll(l, path)
	checkInvariants(l, &c)
}
