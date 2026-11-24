package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
	d, err := os.ReadFile("examples/domain-lab.json")
	if err != nil {
		t.Fatal(err)
	}
	c := config{Dir: t.TempDir(), Retention: 7 * 24 * time.Hour, Cycle: time.Minute, MaxLate: 10 * time.Minute, ClockControl: true, Seed: 73,
		ReadToken: sha256.Sum256([]byte("reader")), AdminToken: sha256.Sum256([]byte("admin"))}
	c.DomainFile = filepath.Join(t.TempDir(), "domain.json")
	if err := os.WriteFile(c.DomainFile, d, 0600); err != nil {
		t.Fatal(err)
	}
	l.store, err = openStore(c, func() time.Time { return l.clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.lock.Close() })
	return l
}

func (l *lab) restart() {
	l.Helper()
	if err := l.lock.Close(); err != nil {
		l.Fatal(err)
	}
	s, err := openStore(l.cfg, func() time.Time { return l.clock })
	if err != nil {
		l.Fatal(err)
	}
	l.store = s
}

func (l *lab) request(method, path, body, token string, status int) *httptest.ResponseRecorder {
	l.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	l.ServeHTTP(w, r)
	if w.Code != status {
		l.Fatalf("%s %s: want %d, got %d: %s", method, path, status, w.Code, w.Body.String())
	}
	header := w.Header().Get("X-Schema-Version")
	if strings.HasPrefix(path, "/health/") {
		if header != "" || w.Body.Len() != 0 {
			l.Fatal("probe leaked schema or body")
		}
	} else if status == 401 {
		if header != "" || w.Body.String() != `{"code":"UNAUTHENTICATED"}` || w.Header().Get("WWW-Authenticate") != "Bearer" {
			l.Fatal("unexpected authentication response")
		}
	} else {
		var b struct{ SchemaVersion string }
		if json.Unmarshal(w.Body.Bytes(), &b) != nil || len(header) != 64 || b.SchemaVersion != header {
			l.Fatalf("schema mismatch: %s", w.Body.String())
		}
	}
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type ledgerRow struct {
	Sequence                                    int64
	RecordedAt                                  time.Time
	Collection, Origin, Kind, Epoch             string
	Lateness, DeleteVisibility                  *string
	EmittedSchemaVersion, RenderedSchemaVersion string
	Item                                        item
	ContentSha256                               string
}

type receipt struct {
	SchemaVersion, PreviousSchemaVersion, FieldsSha256 string
	Rows                                               []ledgerRow
	NextSince                                          int64
	HasMore                                            bool
}

func (l *lab) command(path, body string) receipt {
	l.Helper()
	return decode[receipt](l.T, l.request("POST", "/admin/"+path, body, "admin", 200))
}

func (l *lab) inject(collection, kind, id string) receipt {
	l.Helper()
	b, _ := json.Marshal(object{"collection": collection, "kind": kind, "id": id})
	return l.command("inject", string(b))
}

func (l *lab) page(path string) envelope {
	l.Helper()
	return decode[envelope](l.T, l.request("GET", path, "", "reader", 200))
}

func (l *lab) expected(query string) receipt {
	l.Helper()
	return decode[receipt](l.T, l.request("GET", "/admin/expected?"+query, "", "admin", 200))
}

func TestSnapshotResumeAndRestart(t *testing.T) {
	l := newLab(t)
	l.inject("edicole", "create", "A")
	l.command("inject", `{"collection":"edicole","kind":"late","id":"A","lateness":"PT3M"}`)
	l.clock = l.clock.Add(time.Minute)
	l.inject("edicole", "create", "B")
	p := l.page(itemsURL + "?mode=SNAPSHOT&limit=1")
	l.restart()
	l.command("clock", `{"advance":"PT3M"}`)
	end := l.page(itemsURL + "?mode=SNAPSHOT&page=" + *p.Next)
	if p.Items[0].ID != "A" || p.Items[0].Revision != 1 || end.Items[0].ID != "B" ||
		*p.SnapshotID != *end.SnapshotID || *p.SnapshotComplete || !*end.SnapshotComplete || p.Resume != nil || end.Resume == nil {
		t.Fatal("snapshot moved across a delayed arrival")
	}
	if p.SnapshotAt == nil || end.SnapshotAt == nil || !p.SnapshotAt.Equal(*end.SnapshotAt) || !end.SnapshotAt.Equal(end.Resume.SnapshotAt) ||
		!end.Resume.SnapshotAt.Equal(l.clock) || len(end.Resume.Watermark) != 1 || end.Resume.Watermark["changedSince"] != l.clock.Add(-10*time.Minute).Format(time.RFC3339Nano) {
		t.Fatal("unsafe watermark resume point")
	}
	poll := itemsURL + "?cursor=" + end.Resume.Cursor
	delta := l.page(poll)
	if len(delta.Items) != 1 || delta.Items[0].ID != "A" || delta.Items[0].Revision != 2 || delta.Next == nil {
		t.Fatal("cursor must contain exactly A revision 2")
	}
	q := url.Values{"mode": {"WATERMARK"}, "changedSince": {end.Resume.Watermark["changedSince"]}, "limit": {"1"}}
	var water []item
	for {
		page := l.page(itemsURL + "?" + q.Encode())
		water = append(water, page.Items...)
		if !page.HasMore {
			if page.Next != nil {
				t.Fatal("watermark tail")
			}
			break
		}
		q.Del("changedSince")
		q.Set("cursor", *page.Next)
	}
	if !slices.ContainsFunc(water, func(it item) bool { return reflect.DeepEqual(it, delta.Items[0]) }) ||
		!slices.ContainsFunc(water, func(it item) bool { return it.ID == "B" }) {
		t.Fatal("watermark failed safe replay")
	}
	empty := l.page(itemsURL + "?cursor=" + *delta.Next)
	l.inject("edicole", "update", "B")
	l.restart()
	if replay := l.page(poll); !reflect.DeepEqual(replay.Items, delta.Items) || *replay.Next != *delta.Next {
		t.Fatal("replayed window changed")
	}
	if replay := l.page(itemsURL + "?cursor=" + *delta.Next); len(replay.Items) != 0 || *replay.Next != *empty.Next {
		t.Fatal("first empty poll was not durably frozen")
	}
	if p := l.page(itemsURL + "?cursor=" + *empty.Next); len(p.Items) != 1 || p.Items[0].ID != "B" {
		t.Fatal("empty successor missed an arrival")
	}
}

func TestModesAndAbsence(t *testing.T) {
	l := newLab(t)
	base := "/collections/cantieri/items"
	for _, id := range []string{"A", "B", "C"} {
		l.inject("cantieri", "create", id)
	}
	p := l.page(base + "?limit=1")
	if p.Mode != "SNAPSHOT" || !p.HasMore {
		t.Fatal("incorrect collection default")
	}
	if w := l.request("GET", base+"?mode=CURSOR", "", "reader", 400); !strings.Contains(w.Body.String(), "MODE_NOT_OFFERED") {
		t.Fatal(w.Body.String())
	}
	r := l.inject("cantieri", "delete", "B").Rows[0]
	if r.DeleteVisibility == nil || *r.DeleteVisibility != "SNAPSHOT_ONLY" {
		t.Fatal("missing deletion evidence")
	}
	snap := l.page(base)
	if len(snap.Items) != 2 || snap.Resume.Cursor != "" || snap.Items[1].ID != "C" {
		t.Fatal("invalid snapshot after delete")
	}
	page := l.page(base + "?mode=WATERMARK&limit=1&changedSince=" + l.clock.Format(time.RFC3339))
	total := 0
	for {
		for _, it := range page.Items {
			if it.Op == "DELETE" {
				t.Fatal("hidden tombstone exposed")
			}
			total++
		}
		if !page.HasMore {
			break
		}
		page = l.page(base + "?mode=WATERMARK&cursor=" + *page.Next)
	}
	if total != 3 {
		t.Fatal(total)
	}
	l.request("GET", base+"?mode=WATERMARK&page=invalid", "", "reader", 400)
}

func TestDurableDelayDuplicateAndAtomicity(t *testing.T) {
	l := newLab(t)
	first := l.inject("edicole", "create", "A").Rows[0]
	dup := l.inject("edicole", "duplicate", "A").Rows[0]
	if !reflect.DeepEqual(first.Item, dup.Item) || dup.ContentSha256 != first.ContentSha256 || dup.Kind != "DUPLICATE" {
		t.Fatal("duplicate changed content")
	}
	l.request("POST", "/admin/inject", `{"collection":"edicole","kind":"late","lateness":"PT11M"}`, "admin", 400)
	next := l.command("inject", `{"collection":"edicole","id":"A","kind":"late","lateness":"PT5S"}`)
	if len(next.Rows) != 0 || next.NextSince != dup.Sequence {
		t.Fatal("pending event already visible")
	}
	l.clock = l.clock.Add(time.Second)
	del := l.inject("edicole", "delete", "A").Rows[0]
	if del.Item.Revision != 3 || del.DeleteVisibility == nil || *del.DeleteVisibility != "EXPLICIT" {
		t.Fatal("wrong explicit delete")
	}
	l.restart()
	l.clock = l.clock.Add(4 * time.Second)
	late := l.expected("since=" + decimalSeq(del.Sequence)).Rows[0]
	snapshot := l.page(itemsURL + "?mode=SNAPSHOT")
	if late.Item.Revision != 2 || late.Kind != "LATE_UPDATE" || late.Lateness == nil || *late.Lateness != "PT5S" ||
		!late.Item.ChangedAt.Before(del.Item.ChangedAt) || late.Sequence <= del.Sequence || len(snapshot.Items) != 1 || !reflect.DeepEqual(snapshot.Items[0], del.Item) {
		t.Fatal("late update resurrected retired object")
	}
	l.request("POST", "/admin/inject", `{"kind":"create","collection":"edicole","id":"X","count":2}`, "admin", 409)
	if len(l.expected("since="+decimalSeq(late.Sequence)).Rows) != 0 {
		t.Fatal("partial batch committed")
	}
}

func TestSchemaRenderingAndRevert(t *testing.T) {
	l := newLab(t)
	l.command("inject", `{"collection":"edicole","kind":"create","count":3}`)
	first := l.page(itemsURL + "?limit=1")
	previous := first.Version
	for _, edit := range []string{
		`"action":"add","name":"places","type":"integer"`,
		`"action":"rename","name":"places","newName":"enabled"`,
		`"action":"type","name":"enabled","type":"boolean"`,
		`"action":"nullable","name":"enabled","nullable":true`,
		`"action":"describe","name":"enabled","description":"Flag"`,
		`"action":"remove","name":"telefono"`,
	} {
		r := l.command("schema", `{"collection":"edicole",`+edit+"}")
		page := l.page(itemsURL + "?cursor=" + *first.Next)
		if r.SchemaVersion == previous || r.PreviousSchemaVersion != previous || page.Version != r.SchemaVersion || r.FieldsSha256 != page.FieldsSha256 {
			t.Fatal("schema change missed between pages")
		}
		ledger := l.expected("collection=edicole&renderAs=" + page.Version)
		row := ledger.Rows[1]
		if row.EmittedSchemaVersion != first.Version || row.RenderedSchemaVersion != page.Version || row.ContentSha256 != digest(page.Items[0]) {
			t.Fatal("renderAs differs from delivered content")
		}
		previous = page.Version
	}
	l.restart()
	r := l.command("schema", `{"collection":"edicole","action":"revert","version":"`+first.Version+`"}`)
	if r.SchemaVersion != first.Version || r.FieldsSha256 != first.FieldsSha256 {
		t.Fatal("revert changed fingerprints")
	}
	l.request("GET", "/admin/expected?collection=edicole&renderAs=missing", "", "admin", 400)
}

func TestRetentionClockAndTornLine(t *testing.T) {
	l := newLab(t)
	idle := l.page(itemsURL)
	l.command("clock", `{"advance":"P8D"}`)
	l.inject("edicole", "create", "A")
	l.command("clock", `{"advance":"PT1M"}`)
	l.inject("edicole", "create", "B")
	p := l.page(itemsURL + "?cursor=" + *idle.Next + "&limit=1")
	want := l.now().Add(l.cfg.Retention)
	if p.PositionExpiresAt == nil || !p.PositionExpiresAt.Equal(want) {
		t.Fatal("expiry must refer to first unread arrival")
	}
	l.command("clock", `{"advance":"P6DT23H59M1S"}`)
	// A is compacted, but only B is needed for this page.
	b := l.page(itemsURL + "?cursor=" + *p.Next)
	if len(b.Items) != 1 || b.Items[0].ID != "B" || b.PositionExpiresAt != nil {
		t.Fatal("premature expiry")
	}
	l.command("clock", `{"advance":"PT1M"}`)
	l.expiry(itemsURL+"?cursor="+*p.Next, "reader", "RETENTION")
	l.expiry(itemsURL+"?mode=WATERMARK&changedSince="+url.QueryEscape(l.clock.Format(time.RFC3339)), "reader", "RETENTION")
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"events":[`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	at := l.now()
	l.restart()
	if !l.now().Equal(at) || len(l.page(itemsURL+"?mode=SNAPSHOT").Items) != 2 {
		t.Fatal("compacted checkpoint or clock not restored")
	}
	l.inject("edicole", "update", "B")
	l.restart()
	l.command("reset", "{}")
	l.expiry(itemsURL+"?cursor="+*b.Next, "reader", "EPOCH_CHANGED")
}

func TestCredentialsErrorsAndProbes(t *testing.T) {
	l := newLab(t)
	for _, path := range []string{"/collections", "/collections/edicole/schema", itemsURL, "/admin/status", "/admin/expected", "/admin/expected/state", "/admin/rate", "/admin/inject", "/admin/schema", "/admin/clock", "/admin/outage", "/admin/reset"} {
		wrong := "admin"
		if strings.HasPrefix(path, "/admin") {
			wrong = "reader"
		}
		for _, token := range []string{"", wrong} {
			l.request("GET", path, "", token, 401)
		}
	}
	l.request("GET", "/collections/missing/schema", "", "reader", 404)
	l.command("outage", `{"kind":"UNAVAILABLE","duration":"PT5S","retryAfter":2}`)
	l.restart()
	if w := l.request("GET", "/collections", "", "reader", 503); w.Header().Get("Retry-After") != "2" {
		t.Fatal("Retry-After")
	}
	for _, path := range []string{"live", "ready"} {
		l.request("GET", "/health/"+path, "", "", 204)
	}
	l.command("clock", `{"advance":"PT5S"}`)
	l.command("outage", `{"collection":"cantieri","kind":"RATE_LIMITED","duration":"PT3S"}`)
	l.request("GET", "/collections/cantieri/items", "", "reader", 429)
	l.cfg.ReadInterval = 1500 * time.Millisecond
	l.page(itemsURL)
	if w := l.request("GET", itemsURL, "", "reader", 429); w.Header().Get("Retry-After") != "2" {
		t.Fatal("fractional Retry-After")
	}
	l.cfg.ClockControl = false
	l.request("POST", "/admin/clock", `{"advance":"PT1S"}`, "admin", 404)
}

func TestCanonicalVectors(t *testing.T) {
	if digest([]field{{"Caffè", "id", false, "string"}}) != "9d6a480a969b79b83dc6398f197b32c3c9dffa10e8cdd82dd34fa250fa632610" {
		t.Fatal("fields vector")
	}
	in := json.RawMessage(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/","literals":[null,true,false]}`)
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`
	if got := canonical(in); !bytes.Equal(got, []byte(want)) {
		t.Fatalf("RFC 8785 vector: %s", got)
	}
	if got := string(canonical(json.RawMessage(`{"\ue000":1,"😀":-0}`))); got != `{"😀":0,"":1}` {
		t.Fatal("UTF-16 key order or negative zero")
	}
}

func (l *lab) expiry(path, token, reason string) {
	l.Helper()
	r := decode[struct{ Code, Reason, Epoch string }](l.T, l.request("GET", path, "", token, 410))
	if r.Code != "CURSOR_EXPIRED" || r.Reason != reason || (reason == "EPOCH_CHANGED" && r.Epoch != l.Epoch) {
		l.Fatalf("unexpected expiry: %+v", r)
	}
}

func TestSnapshotRetainedDelete(t *testing.T) {
	l := newLab(t)
	for _, id := range []string{"C", "B", "A"} {
		l.inject("edicole", "create", id)
	}
	deletion := l.inject("edicole", "delete", "B").Rows[0]
	base := itemsURL + "?mode=SNAPSHOT"
	first := l.page(base + "&limit=1")
	expires := deletion.RecordedAt.Add(l.cfg.Retention)
	if len(first.Items) != 1 || first.Items[0].ID != "A" || first.Next == nil ||
		first.PositionExpiresAt == nil || !first.PositionExpiresAt.Equal(expires) {
		t.Fatal("first page must cover A and report the unread DELETE expiry")
	}
	l.command("clock", `{"advance":"P7D"}`)
	w := l.request("GET", base+"&page="+*first.Next, "", "reader", 200)
	middle := decode[envelope](t, w)
	wire := decode[struct{ Items []object }](t, w)
	if len(wire.Items) != 1 || digest(wire.Items[0]) != deletion.ContentSha256 || deletion.Epoch != first.Epoch {
		t.Fatal("snapshot DELETE wire content or epoch differs from its ledger row")
	}
	if !middle.HasMore || middle.Next == nil || middle.SnapshotAt == nil || first.SnapshotAt == nil || !middle.SnapshotAt.Equal(*first.SnapshotAt) ||
		len(middle.Items) != 1 || !reflect.DeepEqual(middle.Items[0], deletion.Item) ||
		middle.Items[0].Op != "DELETE" || middle.Items[0].Properties != nil || middle.PositionExpiresAt != nil {
		t.Fatal("missing retained DELETE at the inclusive retention boundary")
	}
	l.command("clock", `{"advance":"PT1S"}`)
	l.restart()
	l.expiry(base+"&page="+*first.Next, "reader", "RETENTION")
	end := l.page(base + "&page=" + *middle.Next)
	if len(end.Items) != 1 || end.Items[0].ID != "C" || end.HasMore || end.SnapshotComplete == nil || !*end.SnapshotComplete || end.Resume == nil ||
		*first.SnapshotID != *end.SnapshotID || !first.SnapshotAt.Equal(end.Resume.SnapshotAt) {
		t.Fatal("compaction of an already delivered DELETE changed the snapshot")
	}
	fresh := l.page(base)
	if len(fresh.Items) != 2 || fresh.Items[0].ID != "A" || fresh.Items[1].ID != "C" {
		t.Fatal("fresh snapshot must omit the expired DELETE and preserve both live objects")
	}
}

func TestExpectedStateAsOf(t *testing.T) {
	l := newLab(t)
	base := "/admin/expected/state?collection=edicole"
	version := l.page(itemsURL).Version
	read := func(query string, cut int64) []stateRow {
		t.Helper()
		r := decode[struct {
			AsOf                 *int64
			Epoch, SchemaVersion string
			Items                []stateRow
		}](t, l.request("GET", base+query, "", "admin", 200))
		if r.AsOf == nil || *r.AsOf != cut || r.Epoch != l.Epoch || r.SchemaVersion != version {
			t.Fatalf("incorrect state metadata for cut %d", cut)
		}
		return r.Items
	}
	a := l.inject("edicole", "create", "A").Rows[0]
	l.command("clock", `{"advance":"PT1M"}`)
	b := l.inject("edicole", "create", "B").Rows[0]
	deletion := l.inject("edicole", "delete", "A").Rows[0]
	l.inject("edicole", "create", "A")
	if len(read("&asOf=0", 0)) != 0 {
		t.Fatal("cut zero must precede the first arrival")
	}
	l.command("clock", `{"advance":"P6DT23H59M1S"}`)
	l.restart()
	l.expiry(base+"&asOf=0", "admin", "RETENTION")
	atFloor := read("&asOf="+decimalSeq(a.Sequence), a.Sequence)
	if len(atFloor) != 1 || atFloor[0].ID != "A" || atFloor[0].ContentSha256 != a.ContentSha256 {
		t.Fatal("checkpoint state unavailable at the compaction floor")
	}
	items := read("&asOf="+decimalSeq(b.Sequence), b.Sequence)
	if len(items) != 2 || items[0].ContentSha256 != a.ContentSha256 || items[1].ContentSha256 != b.ContentSha256 {
		t.Fatal("historical state must combine checkpoint and retained suffix")
	}
	items = read("&asOf="+decimalSeq(deletion.Sequence), deletion.Sequence)
	if len(items) != 2 || items[0].DeletedAt == nil || !items[0].DeletedAt.Equal(deletion.Item.ChangedAt) || items[0].Revision != deletion.Item.Revision {
		t.Fatal("reactivation leaked into historical state")
	}
	if head := read("", deletion.Sequence+1); len(head) != 2 || head[0].DeletedAt != nil || head[0].Revision != deletion.Item.Revision+1 {
		t.Fatal("state without asOf must reflect reactivation at head")
	}
	for _, bad := range []string{"", "-1", "5", "abc", "1&asOf=2"} {
		r := decode[struct{ Code string }](t, l.request("GET", base+"&asOf="+bad, "", "admin", 400))
		if r.Code != "INVALID_POSITION" {
			t.Fatal("invalid asOf accepted")
		}
	}
}

func decimalSeq(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestSnapshotMatchesStateWithHiddenCursorDeletes(t *testing.T) {
	l := newLab(t)
	d := l.Domain
	d.Collections[1].Delta.Modes = append(d.Collections[1].Delta.Modes, "CURSOR")
	writeDomain(t, l.cfg.DomainFile, d)
	l.command("reset", `{"reloadDomain":true}`)
	for _, id := range []string{"A", "B", "C"} {
		l.inject("cantieri", "create", id)
	}
	l.inject("cantieri", "delete", "B")
	cut := l.inject("cantieri", "update", "C").NextSince
	base := "/collections/cantieri/items"
	changes := l.page(base + "?mode=CURSOR")
	if len(changes.Items) != 4 || slices.ContainsFunc(changes.Items, func(it item) bool { return it.Op == "DELETE" }) {
		t.Fatal("CURSOR exposed a snapshot-only delete")
	}
	want := map[string]item{}
	p := l.page(base + "?limit=1")
	for {
		for _, it := range p.Items {
			want[it.ID] = it
		}
		if !p.HasMore {
			break
		}
		p = l.page(base + "?page=" + *p.Next)
	}
	l.inject("cantieri", "create", "after-snapshot")
	q := "/admin/expected/state?collection=cantieri&asOf=" + decimalSeq(cut) + "&renderAs=" + p.Version + "&limit=1"
	after := ""
	deleted := 0
	for {
		state := decode[struct {
			Items     []stateRow
			NextAfter *string
		}](t, l.request("GET", q+"&after="+url.QueryEscape(after), "", "admin", 200))
		for _, row := range state.Items {
			if row.DeletedAt != nil {
				if row.ID != "B" || row.Revision != 2 {
					t.Fatal("unexpected retained tombstone")
				}
				deleted++
				continue
			}
			it, ok := want[row.ID]
			if !ok || it.Revision != row.Revision || !reflect.DeepEqual(it.Properties, row.Properties) || digest(it) != row.ContentSha256 {
				t.Fatal("snapshot and expected state disagree")
			}
			delete(want, row.ID)
		}
		if state.NextAfter == nil {
			break
		}
		after = *state.NextAfter
	}
	if len(want) != 0 || deleted != 1 {
		t.Fatal("incomplete state")
	}
}

func TestCatalogDigestAndSignedPositions(t *testing.T) {
	l := newLab(t)
	cat := decode[struct {
		SchemaVersion, Epoch string
		Collections          []struct{ Collection, SchemaVersion, Epoch string }
	}](t, l.request("GET", "/collections", "", "reader", 200))
	ep, err := hex.DecodeString(cat.Epoch)
	if err != nil || len(ep) != 16 || hex.EncodeToString(ep) != cat.Epoch {
		t.Fatal("invalid catalog epoch")
	}
	pairs := [][2]string{}
	for _, c := range cat.Collections {
		if c.Epoch != cat.Epoch {
			t.Fatal("collection epoch differs from catalog")
		}
		sc := decode[struct {
			Fields       []field
			Geometry     *geometry
			Key          []string
			FieldsSha256 string
		}](t, l.request("GET", "/collections/"+c.Collection+"/schema", "", "reader", 200))
		if digest(sc.Fields) != sc.FieldsSha256 || digest([]any{sc.Fields, sc.Key, sc.Geometry}) != c.SchemaVersion {
			t.Fatal("incorrect public schema descriptors")
		}
		pairs = append(pairs, [2]string{c.Collection, c.SchemaVersion})
	}
	if cat.SchemaVersion != digest(pairs) || pairs[0][0] != "cantieri" {
		t.Fatal("catalog must hash the sorted pair array")
	}
	l.inject("edicole", "create", "A")
	l.restart()
	status := decode[struct{ Epoch string }](t, l.request("GET", "/admin/status", "", "admin", 200))
	if status.Epoch != cat.Epoch || l.expected("since=0").Rows[0].Epoch != cat.Epoch {
		t.Fatal("journal epoch changed on replay or missing from ledger/status")
	}
	for _, query := range []string{"", "?mode=SNAPSHOT", "?mode=WATERMARK&changedSince=" + l.clock.Format(time.RFC3339)} {
		if l.page(itemsURL+query).Epoch != cat.Epoch {
			t.Fatal("items epoch differs from catalog")
		}
	}
	p := l.page(itemsURL)
	raw, tag, _ := strings.Cut(*p.Next, ".")
	b, _ := b64.DecodeString(raw)
	var forged position
	if err := json.Unmarshal(b, &forged); err != nil {
		t.Fatal(err)
	}
	forged.Epoch = "forged"
	b, _ = json.Marshal(forged)
	l.request("GET", itemsURL+"?cursor="+b64.EncodeToString(b)+"."+tag, "", "reader", 400)
	l.request("GET", itemsURL+"?mode=WATERMARK&cursor="+*p.Next, "", "reader", 400)
	l.command("reset", "{}")
	l.restart()
	if current := l.page(itemsURL).Epoch; current == "" || current == cat.Epoch {
		t.Fatal("reset did not start a new epoch")
	}
	l.expiry(itemsURL+"?cursor="+*p.Next, "reader", "EPOCH_CHANGED")
}
