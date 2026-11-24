package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

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
	Fields     []field              `json:"fields"`
	Key        []string             `json:"key"`
	Geometry   *geometry            `json:"geometry"`
	Origins    map[string]string    `json:"origins"`
	Generators map[string]generator `json:"generators"`
	BBox       []float64            `json:"bbox"`
}

func (s schema) version() string {
	return digest([]any{s.Fields, s.Key, s.Geometry})
}

func (s schema) sort() {
	slices.SortFunc(s.Fields, func(a, b field) int { return strings.Compare(a.Name, b.Name) })
}

func (d definition) schema() schema {
	s := schema{Key: []string{"id"}, Origins: map[string]string{}, Generators: map[string]generator{}}
	if d.Geometry != nil {
		s.Geometry = &d.Geometry.geometry
		s.BBox = d.Geometry.BBox
	}
	for _, f := range d.Fields {
		s.Fields = append(s.Fields, f.field)
		g := f.Generator
		if f.NullPercent != nil {
			g.NullPercent = f.NullPercent
		}
		s.Generators[f.Name] = g
		s.Origins[f.Name] = f.Name
	}
	s.sort()
	return s
}

func (s schema) validate() error {
	seen := map[string]bool{}
	for _, f := range s.Fields {
		if !fieldName.MatchString(f.Name) || seen[f.Name] {
			return fmt.Errorf("fields.%s: invalid or duplicate name", f.Name)
		}
		seen[f.Name] = true
		g := s.Generators[f.Name]
		if err := g.validate(f); err != nil {
			return fmt.Errorf("fields.%s: %w", f.Name, err)
		}
		if f.Name == "id" && f.Nullable {
			return fmt.Errorf("fields.id must be non-nullable")
		}
		if f.Type == "geometry" && (s.Geometry == nil || s.Geometry.Field != f.Name ||
			(g.Kind == "point" && s.Geometry.Type != "Point") || (g.Kind == "polygon" && s.Geometry.Type != "Polygon")) {
			return fmt.Errorf("geometry descriptor must match fields.%s", f.Name)
		}
	}
	if !seen["id"] {
		return fmt.Errorf("fields.id is required")
	}
	if s.Geometry != nil && !slices.Contains([]string{"point", "polygon"}, s.Generators[s.Geometry.Field].Kind) {
		return fmt.Errorf("geometry.field must identify a geometry")
	}
	for name, g := range s.Generators {
		path := map[string]bool{name: true}
		for g.After != "" {
			next := s.Generators[g.After]
			if path[g.After] || next.Kind != "date" || (g.Varies != "REVISION" && next.Varies == "REVISION") {
				return fmt.Errorf("fields.%s: invalid date.after dependency", name)
			}
			path[g.After] = true
			g = next
		}
	}
	return nil
}

type schemaEdit struct {
	Collection  string     `json:"collection"`
	Action      string     `json:"action"`
	Name        string     `json:"name"`
	NewName     string     `json:"newName"`
	Type        string     `json:"type"`
	Nullable    *bool      `json:"nullable"`
	Description *string    `json:"description"`
	Generator   *generator `json:"generator"`
	Version     string     `json:"version"`
}

func defaultGenerator(kind string) generator {
	lo, hi, scale := 0.0, 100.0, 2
	switch kind {
	case "string":
		return generator{Kind: "template", Pattern: "val-{int:1..10000}"}
	case "integer":
		return generator{Kind: kind, Min: &lo, Max: &hi}
	case "decimal":
		return generator{Kind: kind, Min: &lo, Max: &hi, Scale: &scale}
	case "date":
		return generator{Kind: kind, From: "2000-01-01", To: "2030-12-31"}
	case "datetime":
		return generator{Kind: kind, Source: "CHANGED_AT"}
	case "geometry":
		return generator{Kind: "point"}
	default:
		return generator{Kind: kind}
	}
}

func editSchema(old schema, e schemaEdit) (schema, error) {
	s := old
	s.Fields = slices.Clone(old.Fields)
	s.Origins = maps.Clone(old.Origins)
	s.Generators = maps.Clone(old.Generators)
	if old.Geometry != nil {
		g := *old.Geometry
		s.Geometry = &g
	}
	i := slices.IndexFunc(s.Fields, func(f field) bool { return f.Name == e.Name })
	if !fieldName.MatchString(e.Name) || (e.Name == "id" && e.Action != "describe") || (i < 0) != (e.Action == "add") {
		return s, fmt.Errorf("invalid field for action")
	}
	switch e.Action {
	case "add", "type":
		if e.Action == "add" {
			i = len(s.Fields)
			s.Fields = append(s.Fields, field{Name: e.Name})
			s.Origins[e.Name] = e.Name
		}
		f := &s.Fields[i]
		f.Type = e.Type
		if e.Description != nil {
			f.Description = *e.Description
		}
		if e.Nullable != nil {
			f.Nullable = *e.Nullable
		}
		g := defaultGenerator(e.Type)
		if e.Generator != nil {
			g = *e.Generator
		}
		s.Generators[e.Name] = g
		if s.Geometry != nil && s.Geometry.Field == e.Name && e.Type != "geometry" {
			s.Geometry = nil
		}
		if e.Type == "geometry" && s.Geometry == nil {
			shape := "Point"
			if g.Kind == "polygon" {
				shape = "Polygon"
			}
			s.Geometry = &geometry{e.Name, shape, "EPSG:4326"}
			if len(s.BBox) == 0 {
				s.BBox = []float64{-1, -1, 1, 1}
			}
		}
	case "remove":
		s.Fields = slices.Delete(s.Fields, i, i+1)
		delete(s.Origins, e.Name)
		delete(s.Generators, e.Name)
		if s.Geometry != nil && s.Geometry.Field == e.Name {
			s.Geometry = nil
		}
	case "rename":
		if _, exists := s.Generators[e.NewName]; exists || !fieldName.MatchString(e.NewName) {
			return s, fmt.Errorf("invalid newName")
		}
		s.Fields[i].Name = e.NewName
		s.Origins[e.NewName] = s.Origins[e.Name]
		s.Generators[e.NewName] = s.Generators[e.Name]
		delete(s.Origins, e.Name)
		delete(s.Generators, e.Name)
		for k, g := range s.Generators {
			if g.After == e.Name {
				g.After = e.NewName
				s.Generators[k] = g
			}
		}
		if s.Geometry != nil && s.Geometry.Field == e.Name {
			s.Geometry.Field = e.NewName
		}
	case "nullable":
		if e.Nullable == nil {
			return s, fmt.Errorf("nullable is required")
		}
		s.Fields[i].Nullable = *e.Nullable
		if !*e.Nullable {
			g := s.Generators[e.Name]
			g.NullPercent = nil
			s.Generators[e.Name] = g
		}
	case "describe":
		if e.Description == nil {
			return s, fmt.Errorf("description is required")
		}
		s.Fields[i].Description = *e.Description
	default:
		return s, fmt.Errorf("unknown schema action")
	}
	s.sort()
	return s, s.validate()
}
