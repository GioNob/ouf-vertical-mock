package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Key order is part of the fields fingerprint.
type field struct {
	Description string `json:"description"`
	Name        string `json:"name"`
	Nullable    bool   `json:"nullable"`
	Type        string `json:"type"`
}

type geometry struct {
	Field string `json:"field"`
	Type  string `json:"type"`
	CRS   string `json:"crs"`
}

type schema struct {
	Fields   []field           `json:"fields"`
	Key      []string          `json:"key"`
	Geometry *geometry         `json:"geometry"`
	Origins  map[string]string `json:"origins"` // source attribute after a rename
}

// Preserve literal backslashes while undoing the encoder's JSONP-only escapes.
var canonicalEscapes = strings.NewReplacer(`\\`, `\\`, `\u2028`, "\u2028", `\u2029`, "\u2029")

func digest(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only JSON-native schema descriptors enter this function
	}
	sum := sha256.Sum256([]byte(canonicalEscapes.Replace(strings.TrimSuffix(b.String(), "\n"))))
	return hex.EncodeToString(sum[:])
}

func (s schema) version() string {
	return digest([]any{s.Fields, s.Key, s.Geometry})
}

func initialSchema(name string) schema {
	s := schema{Key: []string{"id"}, Origins: map[string]string{}}
	add := func(name, kind, description string) {
		s.Fields = append(s.Fields, field{description, name, false, kind})
	}
	add("id", "string", "Identificativo naturale")
	add("geometria", "geometry", "Geometria in longitudine e latitudine")
	s.Geometry = &geometry{"geometria", "Point", "EPSG:4326"}
	if name == "edicole" {
		add("indirizzo", "string", "Via e numero civico")
		add("cap", "string", "Codice di avviamento postale")
		add("localita", "string", "Località fittizia")
		add("tipologia", "string", "Tipo di rivendita")
		add("servizi", "string", "Servizi disponibili")
		add("telefono", "string", "Recapito dimostrativo")
		add("data_apertura", "date", "Data di apertura")
	} else {
		s.Geometry.Type = "Polygon"
		add("intervento", "string", "Tipo di intervento")
		add("data_inizio", "date", "Inizio lavori")
		add("data_fine", "date", "Fine lavori prevista")
	}
	sortFields(s.Fields)
	return s
}

func sortFields(fields []field) {
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
}

type schemaEdit struct {
	Collection  string `json:"collection"`
	Action      string `json:"action"`
	Name        string `json:"name"`
	NewName     string `json:"newName"`
	Type        string `json:"type"`
	Nullable    bool   `json:"nullable"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func editSchema(old schema, e schemaEdit) (schema, error) {
	s := schema{Fields: slices.Clone(old.Fields), Key: old.Key, Origins: map[string]string{}}
	for k, v := range old.Origins {
		s.Origins[k] = v
	}
	if old.Geometry != nil {
		g := *old.Geometry
		s.Geometry = &g
	}
	if !fieldName.MatchString(e.Name) || e.Name == "id" {
		return s, fmt.Errorf("invalid or reserved field name")
	}
	index := func(name string) int {
		return slices.IndexFunc(s.Fields, func(f field) bool { return f.Name == name })
	}
	i := index(e.Name)
	if e.Action == "add" || e.Action == "type" {
		if !slices.Contains([]string{"string", "integer", "decimal", "boolean", "date", "datetime", "geometry"}, e.Type) {
			return s, fmt.Errorf("unknown field type")
		}
	}
	if e.Action != "add" && i < 0 {
		return s, fmt.Errorf("unknown field")
	}
	switch e.Action {
	case "add":
		if i >= 0 {
			return s, fmt.Errorf("field already exists")
		}
		s.Fields = append(s.Fields, field{e.Description, e.Name, e.Nullable, e.Type})
	case "remove":
		s.Fields = slices.Delete(s.Fields, i, i+1)
		delete(s.Origins, e.Name)
	case "rename":
		if !fieldName.MatchString(e.NewName) || index(e.NewName) >= 0 {
			return s, fmt.Errorf("invalid new field name")
		}
		origin := s.Origins[e.Name]
		if origin == "" {
			origin = e.Name
		}
		delete(s.Origins, e.Name)
		s.Origins[e.NewName] = origin
		s.Fields[i].Name = e.NewName
		if s.Geometry != nil && s.Geometry.Field == e.Name {
			s.Geometry.Field = e.NewName
		}
	case "type":
		s.Fields[i].Type = e.Type
	default:
		return s, fmt.Errorf("unknown schema action")
	}
	if s.Geometry != nil && s.Geometry.Field == e.Name && (e.Action == "remove" || (e.Action == "type" && e.Type != "geometry")) {
		s.Geometry = nil
	}
	if (e.Action == "type" || e.Action == "add") && e.Type == "geometry" && s.Geometry == nil {
		s.Geometry = &geometry{e.Name, "Point", "EPSG:4326"}
	}
	sortFields(s.Fields)
	return s, nil
}

type item struct {
	ID         string         `json:"id"`
	Revision   int64          `json:"revision"`
	Op         string         `json:"op"`
	ChangedAt  time.Time      `json:"changedAt"`
	Properties map[string]any `json:"properties,omitempty"`
}

func sample(seed int64, key string, n int64) uint64 {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d/%s/%d", seed, key, n))
	return binary.BigEndian.Uint64(sum[:8])
}

func render(seed int64, r record, s schema) item {
	it := item{ID: r.ID, Revision: r.Revision, Op: r.Op, ChangedAt: r.ChangedAt}
	if it.Op == "DELETE" {
		return it
	}
	it.Properties = make(map[string]any, len(s.Fields))
	place := sample(seed, r.ID, 0)
	value := sample(seed, r.ID, r.Revision)
	x := 12.0 + float64(place%9000)/100000
	y := 45.0 + float64((place/9000)%9000)/100000
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(place%365))
	for _, f := range s.Fields {
		name := s.Origins[f.Name]
		if name == "" {
			name = f.Name
		}
		var v any
		switch f.Type {
		case "string":
			switch name {
			case "id":
				v = r.ID
			case "indirizzo":
				v = fmt.Sprintf("%s %d", []string{"Via delle Lanterne", "Via dei Gelsi", "Piazza della Carta"}[place%3], 1+place%120)
			case "cap":
				v = fmt.Sprintf("35%03d", place%100)
			case "localita":
				v = "Borgo delle Lanterne"
			case "tipologia":
				v = []string{"chiosco", "rivendita in negozio"}[place%2]
			case "servizi":
				v = []string{"quotidiani e riviste", "riviste e cartoleria", "quotidiani e biglietteria"}[value%3]
			case "telefono":
				v = fmt.Sprintf("+39 000 %07d", place%10000000)
			case "intervento":
				v = []string{"Rifacimento marciapiede", "Posa di condotte", "Manutenzione del verde"}[value%3]
			default:
				v = fmt.Sprintf("%s-%04d", name, value%10000)
			}
		case "integer":
			v = int(value % 10000)
		case "decimal":
			v = float64(value%100000) / 100
		case "boolean":
			v = value%2 == 0
		case "date":
			date := start
			if name == "data_fine" {
				date = date.AddDate(0, 0, 10+int(value%80))
			}
			v = date.Format(time.DateOnly)
		case "datetime":
			v = r.ChangedAt
		case "geometry":
			if s.Geometry != nil && s.Geometry.Field == f.Name && s.Geometry.Type == "Polygon" {
				ring := [][]float64{{x, y}, {x + .001, y}, {x + .001, y + .001}, {x, y + .001}, {x, y}}
				v = map[string]any{"type": "Polygon", "coordinates": [][][]float64{ring}}
			} else {
				v = map[string]any{"type": "Point", "coordinates": []float64{x, y}}
			}
		}
		it.Properties[f.Name] = v
	}
	return it
}
