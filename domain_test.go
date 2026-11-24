package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeDomain(t *testing.T, path string, d domain) {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDomainValidation(t *testing.T) {
	l := newLab(t)
	for name, mutate := range map[string]func(*definition){
		"collection": func(c *definition) { c.Name = "../other" },
		"field":      func(c *definition) { c.Fields[1].Name = c.Fields[0].Name },
		"id":         func(c *definition) { c.Fields[0].Nullable = true },
		"default":    func(c *definition) { c.Delta.Default = "missing" },
		"absence": func(c *definition) {
			c.Delta.Modes, c.Delta.Deletions = []string{"CURSOR"}, "SNAPSHOT_ABSENCE"
		},
		"weights":  func(c *definition) { c.Generation.Update = 80 },
		"late":     func(c *definition) { c.Generation.MaxLateness = "PT11M" },
		"bbox":     func(c *definition) { c.Geometry.BBox[0] = c.Geometry.BBox[2] },
		"geometry": func(c *definition) { c.Geometry.Type = "Polygon" },
		"kind":     func(c *definition) { c.Fields[1].Generator.Kind = "script" },
		"varies":   func(c *definition) { c.Fields[1].Generator.Varies = "RANDOM" },
		"template": func(c *definition) { c.Fields[1].Generator.Pattern = "{int:5..1}" },
		"null": func(c *definition) {
			n := 0
			c.Fields[1].Generator.NullPercent = &n
		},
		"dependency": func(c *definition) {
			n := 0
			c.Fields[1].Type = "date"
			c.Fields[1].Generator = generator{Kind: "date", After: c.Fields[1].Name, MinDays: &n, MaxDays: &n}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := loadDomain(l.cfg.DomainFile, l.cfg.MaxLate)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&d.Collections[0])
			path := filepath.Join(t.TempDir(), "bad.json")
			writeDomain(t, path, d)
			if _, err := loadDomain(path, l.cfg.MaxLate); err == nil {
				t.Fatal("invalid domain accepted")
			}
		})
	}
}

func TestDomainAuthorityAndSingleWriter(t *testing.T) {
	l := newLab(t)
	if s, err := openStore(l.cfg, time.Now); err == nil {
		s.lock.Close()
		t.Fatal("second writer acquired lock")
	}
	d, err := loadDomain(l.cfg.DomainFile, l.cfg.MaxLate)
	if err != nil {
		t.Fatal(err)
	}
	d.Collections[0].Name = "other"
	writeDomain(t, l.cfg.DomainFile, d)
	l.restart()
	status := decode[object](t, l.request("GET", "/admin/status", "", "admin", 200))
	if status["domainFileSha256"] == status["journalDomainSha256"] {
		t.Fatal("domain change not reported")
	}
	l.command("reset", "{}")
	l.inject("edicole", "create", "A")
	l.command("reset", `{"reloadDomain":true}`)
	l.request("GET", itemsURL, "", "reader", 404)
	l.inject("other", "create", "A")
	status = decode[object](t, l.request("GET", "/admin/status", "", "admin", 200))
	if status["domainFileSha256"] != status["journalDomainSha256"] {
		t.Fatal("reload did not adopt domain")
	}
}

func TestDeterministicGenerationAndProjection(t *testing.T) {
	a, b := newLab(t), newLab(t)
	for _, l := range []*lab{a, b} {
		l.command("rate", `{"count":2,"create":100,"update":0,"delete":0,"cycle":"PT2S"}`)
		if err := l.cycle(); err != nil {
			t.Fatal(err)
		}
		l.command("rate", `{"collection":"edicole","count":2,"late":100,"duplicate":100,"maxLateness":"PT1S"}`)
		if err := l.cycle(); err != nil {
			t.Fatal(err)
		}
		if len(l.Pending) != 2 {
			t.Fatal("automatic late percentage")
		}
		l.clock = l.clock.Add(time.Second)
		if err := l.cycle(); err != nil {
			t.Fatal(err)
		}
	}
	ra, rb := a.expected("collection=edicole"), b.expected("collection=edicole")
	// The epoch is random by contract (G10): determinism covers everything else.
	if len(ra.Rows) != len(rb.Rows) {
		t.Fatal("same seed and clock diverged")
	}
	for i := range rb.Rows {
		if rb.Rows[i].Epoch == "" || ra.Rows[i].Epoch == "" {
			t.Fatal("register row without epoch")
		}
		rb.Rows[i].Epoch = ra.Rows[i].Epoch
	}
	if !reflect.DeepEqual(ra, rb) {
		t.Fatal("same seed and clock diverged")
	}
	counts := map[string]int{}
	for _, r := range ra.Rows {
		counts[r.Kind]++
		if r.Origin != "GENERATED" {
			t.Fatal("wrong origin")
		}
	}
	if counts["LATE_UPDATE"] != 2 || counts["DUPLICATE"] != 4 {
		t.Fatal(counts)
	}
	for _, e := range a.page("/collections/cantieri/items").Items {
		p := e.Properties
		if p["data_inizio"].(string) >= p["data_fine"].(string) || p["geometria"].(map[string]any)["type"] != "Polygon" {
			t.Fatal("invalid date or polygon")
		}
	}
	a.command("schema", `{"collection":"edicole","action":"add","type":"decimal","name":"cost","nullable":true,"generator":{"kind":"decimal","min":1.0,"max":2.0,"scale":2,"nullPercent":100,"varies":"REVISION"}}`)
	if v, ok := a.page(itemsURL).Items[0].Properties["cost"]; !ok || v != nil {
		t.Fatal("nullPercent ignored")
	}
}

func TestConfiguration(t *testing.T) {
	dir := t.TempDir()
	for k, v := range map[string]string{"PORT": "8080", "DATA_DIR": dir, "DOMAIN_FILE": "examples/domain-lab.json", "CYCLE": "PT1M", "MAX_LATENESS": "PT10M",
		"RETENTION": "P1DT2H3M4.5S", "READ_MIN_INTERVAL": "PT0S", "CLOCK_CONTROL": "disabled", "SEED": "-12"} {
		t.Setenv("OUF_MOCK_"+k, v)
	}
	for _, k := range []string{"READ_TOKEN_FILE", "ADMIN_TOKEN_FILE"} {
		path := filepath.Join(dir, k)
		if err := os.WriteFile(path, []byte("test-"+k+"\r\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OUF_MOCK_"+k, path)
	}
	c, err := loadConfig()
	if err != nil || c.Seed != -12 || c.Retention != 26*time.Hour+3*time.Minute+4500*time.Millisecond {
		t.Fatalf("config: %v", err)
	}
	for k, v := range map[string]string{"RETENTION": "PT10M", "PORT": "0", "CYCLE": "1s", "SEED": "x", "DATA_DIR": "", "DOMAIN_FILE": "",
		"MAX_LATENESS": "P", "CLOCK_CONTROL": "true", "READ_MIN_INTERVAL": "PT", "ADMIN_TOKEN_FILE": filepath.Join(dir, "READ_TOKEN_FILE")} {
		t.Run(k, func(t *testing.T) {
			t.Setenv("OUF_MOCK_"+k, v)
			_, err := loadConfig()
			if err == nil || strings.Contains(err.Error(), "test-") {
				t.Fatal("invalid or secret-bearing configuration diagnostic")
			}
		})
	}
}

func TestScheduledGenerationDuringOutage(t *testing.T) {
	l := newLab(t)
	l.command("rate", `{"cycle":"PT1S","count":1,"create":100,"update":0,"delete":0}`)
	l.command("outage", `{"kind":"UNAVAILABLE","duration":"PT5M"}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.schedule(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows := l.expected("collection=edicole").Rows; len(rows) > 0 {
			l.request("GET", itemsURL, "", "reader", 503)
			if rows[0].Origin != "GENERATED" {
				t.Fatal("outage interrupted generator")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no scheduled event")
}

func TestGeneratedTombstoneBeforeDelayedEmission(t *testing.T) {
	l := newLab(t)
	l.command("rate", `{"count":0}`)
	l.inject("edicole", "create", "A")
	l.command("rate", `{"collection":"edicole","count":1,"late":100,"maxLateness":"PT1S"}`)
	if err := l.cycle(); err != nil {
		t.Fatal(err)
	}
	status := decode[object](t, l.request("GET", "/admin/status", "", "admin", 200))
	if status["pending"] != float64(1) {
		t.Fatal("missing pending revision")
	}
	l.clock = l.clock.Add(time.Millisecond)
	l.command("rate", `{"collection":"edicole","late":0,"create":0,"update":0,"delete":100}`)
	if err := l.cycle(); err != nil {
		t.Fatal(err)
	}
	v := l.command("schema", `{"collection":"edicole","action":"add","name":"flag","type":"boolean"}`).SchemaVersion
	l.command("clock", `{"advance":"PT1S"}`)
	rows := l.expected("collection=edicole").Rows
	if len(rows) != 3 || rows[1].Kind != "DELETE" || rows[1].Item.Revision != 3 || rows[2].Item.Revision != 2 ||
		rows[2].Kind != "LATE_UPDATE" || rows[2].EmittedSchemaVersion != v || rows[2].RenderedSchemaVersion != v {
		t.Fatal("wrong arrival order or schema at emission")
	}
}

func TestDomainNumericEndpoints(t *testing.T) {
	minimum, scale := 0.29, 2
	g := generator{Kind: "decimal", Min: &minimum, Max: &minimum, Scale: &scale}
	if err := g.validate(field{Name: "cost", Type: "decimal"}); err != nil {
		t.Fatal(err)
	}
	if lo, hi, factor := g.numericBounds(); lo != 29 || hi != 29 || factor != 100 {
		t.Fatalf("scaled domain bounds: %d %d %v", lo, hi, factor)
	}
}
