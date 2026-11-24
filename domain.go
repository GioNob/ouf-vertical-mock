package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const domainFormat = "ouf.vertical-mock-domain.v1"

type domain struct {
	fileHash    string
	Format      string       `json:"schema"`
	Collections []definition `json:"collections"`
}

type definition struct {
	Name       string        `json:"collection"`
	Title      string        `json:"title"`
	Prefix     string        `json:"idPrefix"`
	Delta      delta         `json:"delta"`
	Generation rate          `json:"generation"`
	Geometry   *area         `json:"geometry"`
	Fields     []sourceField `json:"fields"`
}

type delta struct {
	Modes     []string `json:"modes"`
	Default   string   `json:"defaultMode"`
	Deletions string   `json:"deletions"`
}

type rate struct {
	Count       int    `json:"count"`
	Create      int    `json:"create"`
	Update      int    `json:"update"`
	Delete      int    `json:"delete"`
	Duplicate   int    `json:"duplicate"`
	Late        int    `json:"late"`
	MaxLateness string `json:"maxLateness"`
}

func (r rate) validate(maxLate time.Duration) error {
	if r.Count < 0 || r.Count > 10000 {
		return fmt.Errorf("count must be in 0..10000")
	}
	for _, p := range []int{r.Create, r.Update, r.Delete, r.Duplicate, r.Late} {
		if p < 0 || p > 100 {
			return fmt.Errorf("percentages must be in 0..100")
		}
	}
	if r.Create+r.Update+r.Delete != 100 {
		return fmt.Errorf("create, update and delete must sum to 100")
	}
	d, err := duration(r.MaxLateness)
	if err != nil || d > maxLate || (r.Late > 0 && d < time.Second) {
		return fmt.Errorf("invalid maxLateness")
	}
	return nil
}

type area struct {
	geometry
	BBox []float64 `json:"bbox"`
}

type sourceField struct {
	field
	NullPercent *int      `json:"nullPercent,omitempty"`
	Generator   generator `json:"generator"`
}

type generator struct {
	Kind        string   `json:"kind"`
	Varies      string   `json:"varies,omitempty"`
	NullPercent *int     `json:"nullPercent,omitempty"`
	Values      []string `json:"values,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Scale       *int     `json:"scale,omitempty"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	After       string   `json:"after,omitempty"`
	MinDays     *int     `json:"minDays,omitempty"`
	MaxDays     *int     `json:"maxDays,omitempty"`
	Source      string   `json:"source,omitempty"`
}

var (
	collectionName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	fieldName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	crsName        = regexp.MustCompile(`^EPSG:[1-9][0-9]*$`)
	placeholder    = regexp.MustCompile(`\{(choice:[^{}]+|int:-?[0-9]+\.\.-?[0-9]+)\}`)
)

func strictJSON(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// Check presence independently of Go's zero values (false and 0 are valid).
func required(raw json.RawMessage, names string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for _, k := range strings.Fields(names) {
		if len(m[k]) == 0 || string(m[k]) == "null" {
			return nil, fmt.Errorf("%s is required", k)
		}
	}
	return m, nil
}

// Go accepts repeated object keys and decodes null scalar values as zero.
// Neither is a valid domain declaration. Only geometry can explicitly be null.
func domainTokens(d *json.Decoder, path string, nullable bool) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case nil:
		if !nullable {
			return fmt.Errorf("%s cannot be null", path)
		}
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return fmt.Errorf("%s: invalid or duplicate key %v", path, key)
			}
			seen[k] = true
			if err := domainTokens(d, path+"."+k, k == "geometry"); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for i := 0; d.More(); i++ {
			if err := domainTokens(d, fmt.Sprintf("%s[%d]", path, i), false); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func domainShape(b []byte) error {
	if err := domainTokens(json.NewDecoder(bytes.NewReader(b)), "domain", false); err != nil {
		return err
	}
	var raw struct {
		Collections []json.RawMessage
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for i, c := range raw.Collections {
		m, err := required(c, "collection title idPrefix fields delta generation")
		if err != nil {
			return fmt.Errorf("collections[%d]: %w", i, err)
		}
		if _, err := required(m["generation"], "count create update delete duplicate late maxLateness"); err != nil {
			return fmt.Errorf("collections[%d].generation: %w", i, err)
		}
		var fields []json.RawMessage
		if err := json.Unmarshal(m["fields"], &fields); err != nil {
			return err
		}
		for j, f := range fields {
			if _, err := required(f, "name type nullable description generator"); err != nil {
				return fmt.Errorf("collections[%d].fields[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

func loadDomain(path string, maxLate time.Duration) (domain, error) {
	var d domain
	b, err := os.ReadFile(path)
	if err != nil {
		return d, fmt.Errorf("OUF_MOCK_DOMAIN_FILE: %w", err)
	}
	d.fileHash = hashBytes(b)
	if !utf8.Valid(b) {
		return d, fmt.Errorf("domain requires UTF-8")
	}
	if err := strictJSON(b, &d); err != nil {
		return d, fmt.Errorf("domain: %w", err)
	}
	if err := domainShape(b); err != nil {
		return d, err
	}
	if d.Format != domainFormat || len(d.Collections) == 0 {
		return d, fmt.Errorf("domain requires %s and at least one collection", domainFormat)
	}
	seen := map[string]bool{}
	for i, c := range d.Collections {
		if !collectionName.MatchString(c.Name) || seen[c.Name] || strings.TrimSpace(c.Title) == "" {
			return d, fmt.Errorf("collections[%d]: invalid or duplicate collection, or empty title", i)
		}
		seen[c.Name] = true
		if err := c.validate(maxLate); err != nil {
			return d, fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return d, nil
}

func (c definition) validate(maxLate time.Duration) error {
	modes := map[string]bool{}
	for _, mode := range c.Delta.Modes {
		if !slices.Contains([]string{"CURSOR", "SNAPSHOT", "WATERMARK"}, mode) || modes[mode] {
			return fmt.Errorf("delta.modes: unknown or duplicate mode")
		}
		modes[mode] = true
	}
	if !modes[c.Delta.Default] || (c.Delta.Deletions != "EXPLICIT" && !(c.Delta.Deletions == "SNAPSHOT_ABSENCE" && modes["SNAPSHOT"])) {
		return fmt.Errorf("invalid delta.defaultMode or delta.deletions")
	}
	if err := c.Generation.validate(maxLate); err != nil {
		return fmt.Errorf("generation: %w", err)
	}
	if a := c.Geometry; a != nil {
		if len(a.BBox) != 4 || a.BBox[0] >= a.BBox[2] || a.BBox[1] >= a.BBox[3] ||
			math.IsInf(a.BBox[2]-a.BBox[0], 0) || math.IsInf(a.BBox[3]-a.BBox[1], 0) || !crsName.MatchString(a.CRS) {
			return fmt.Errorf("geometry requires EPSG CRS and ordered bbox")
		}
	}
	for _, f := range c.Fields {
		if f.NullPercent != nil && f.Generator.NullPercent != nil {
			return fmt.Errorf("fields.%s.nullPercent specified twice", f.Name)
		}
	}
	return c.schema().validate()
}

func (g *generator) UnmarshalJSON(b []byte) error {
	type plain generator
	var value plain
	if err := strictJSON(b, &value); err != nil {
		return err
	}
	allowed := map[string]string{
		"id": "", "choice": "values", "template": "pattern", "integer": "min max", "decimal": "min max scale",
		"boolean": "", "date": "from to after minDays maxDays", "datetime": "source from to", "point": "", "polygon": "",
	}
	params, ok := allowed[value.Kind]
	if !ok {
		return fmt.Errorf("generator.kind: unknown value")
	}
	m, err := required(b, "kind")
	if err != nil {
		return err
	}
	keys := strings.Fields("kind varies nullPercent " + params)
	for k, v := range m {
		if !slices.Contains(keys, k) || string(v) == "null" {
			return fmt.Errorf("generator.%s: invalid parameter for %s", k, value.Kind)
		}
	}
	if _, exists := m["varies"]; exists && value.Varies == "" {
		return fmt.Errorf("generator.varies must be OBJECT or REVISION")
	}
	*g = generator(value)
	return nil
}

func (g generator) numericBounds() (lo, hi int64, factor float64) {
	factor = 1
	if g.Scale != nil {
		factor = math.Pow10(*g.Scale)
	}
	lo = int64(math.Ceil(*g.Min * factor))
	hi = int64(math.Floor(*g.Max * factor))
	// Multiplying a binary64 approximation of a decimal endpoint can move
	// it just off an integer (e.g. 0.29 at scale 2). Compare as delivered.
	if float64(lo-1)/factor >= *g.Min {
		lo--
	} else if float64(lo)/factor < *g.Min {
		lo++
	}
	if float64(hi+1)/factor <= *g.Max {
		hi++
	} else if float64(hi)/factor > *g.Max {
		hi--
	}
	return lo, hi, factor
}

func (g generator) validate(f field) error {
	validType := map[string]string{"id": "string", "choice": "string", "template": "string", "integer": "integer",
		"decimal": "decimal", "boolean": "boolean", "date": "date", "datetime": "datetime", "point": "geometry", "polygon": "geometry"}
	if validType[g.Kind] == "" || validType[g.Kind] != f.Type || ((g.Kind == "id") != (f.Name == "id")) {
		return fmt.Errorf("generator.kind incompatible with field")
	}
	if g.Varies != "" && g.Varies != "OBJECT" && g.Varies != "REVISION" {
		return fmt.Errorf("generator.varies must be OBJECT or REVISION")
	}
	if g.NullPercent != nil && (!f.Nullable || *g.NullPercent < 0 || *g.NullPercent > 100) {
		return fmt.Errorf("generator.nullPercent requires nullable and 0..100")
	}
	switch g.Kind {
	case "choice":
		if len(g.Values) == 0 {
			return fmt.Errorf("choice requires values")
		}
	case "template":
		if g.Pattern == "" || strings.ContainsAny(placeholder.ReplaceAllString(g.Pattern, ""), "{}") {
			return fmt.Errorf("invalid template.pattern")
		}
		for _, p := range placeholder.FindAllString(g.Pattern, -1) {
			if strings.HasPrefix(p, "{int:") {
				lo, hi, _ := strings.Cut(p[5:len(p)-1], "..")
				a, e1 := strconv.ParseInt(lo, 10, 32)
				b, e2 := strconv.ParseInt(hi, 10, 32)
				if e1 != nil || e2 != nil || a > b {
					return fmt.Errorf("invalid template integer interval")
				}
			}
		}
	case "integer", "decimal":
		if g.Min == nil || g.Max == nil || *g.Min > *g.Max {
			return fmt.Errorf("numeric generator requires ordered min and max")
		}
		scale := 0
		if g.Kind == "decimal" {
			if g.Scale == nil || *g.Scale < 0 || *g.Scale > 308 {
				return fmt.Errorf("decimal.scale must be in 0..308")
			}
			scale = *g.Scale
		} else if math.Trunc(*g.Min) != *g.Min || math.Trunc(*g.Max) != *g.Max {
			return fmt.Errorf("integer requires integral bounds")
		}
		factor := math.Pow10(scale)
		if math.Max(math.Abs(*g.Min), math.Abs(*g.Max))*factor >= 1<<53 {
			return fmt.Errorf("scaled numeric bounds exceed the exact integer range")
		}
		if lo, hi, _ := g.numericBounds(); lo > hi {
			return fmt.Errorf("numeric range has no representable scaled value")
		}
	case "date", "datetime":
		layout := time.DateOnly
		if g.Kind == "datetime" {
			if g.Source == "CHANGED_AT" && g.Varies != "OBJECT" && g.From == "" && g.To == "" {
				return nil
			}
			if g.Source != "RANGE" {
				return fmt.Errorf("datetime.source requires RANGE or revision-based CHANGED_AT")
			}
			layout = time.RFC3339Nano
		}
		if g.After == "" && (g.MinDays != nil || g.MaxDays != nil) {
			return fmt.Errorf("minDays and maxDays require after")
		}
		if g.Kind == "date" && g.After != "" {
			if g.From != "" || g.To != "" || g.MinDays == nil || g.MaxDays == nil || *g.MinDays < 0 || *g.MaxDays < *g.MinDays || *g.MaxDays > 365000 {
				return fmt.Errorf("invalid relative date bounds")
			}
		} else {
			a, e1 := time.Parse(layout, g.From)
			b, e2 := time.Parse(layout, g.To)
			if e1 != nil || e2 != nil || b.Before(a) || a.UTC().Year() < 0 || b.UTC().Year() > 9999 {
				return fmt.Errorf("invalid date range")
			}
		}
	}
	return nil
}
