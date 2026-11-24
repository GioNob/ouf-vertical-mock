package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode/utf16"
)

// Go's JSON number encoder uses the ECMAScript binary64 representation.
// Decode through binary64 also for numeric lexemes supplied as RawMessage.
func canonical(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		panic(err)
	}
	var b bytes.Buffer
	var emit func(any)
	emit = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			keys := slices.Collect(maps.Keys(v))
			slices.SortFunc(keys, func(a, b string) int {
				return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
			})
			b.WriteByte('{')
			for i, k := range keys {
				if i != 0 {
					b.WriteByte(',')
				}
				emit(k)
				b.WriteByte(':')
				emit(v[k])
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, x := range v {
				if i != 0 {
					b.WriteByte(',')
				}
				emit(x)
			}
			b.WriteByte(']')
		case string:
			b.WriteByte('"')
			for _, r := range v {
				switch r {
				case '"', '\\':
					b.WriteByte('\\')
					b.WriteRune(r)
				case '\b', '\t', '\n', '\f', '\r':
					b.WriteByte('\\')
					b.WriteByte("btnfr"[strings.IndexRune("\b\t\n\f\r", r)])
				default:
					if r < 32 {
						const digits = "0123456789abcdef"
						b.WriteString(`\u00`)
						b.WriteByte(digits[r>>4])
						b.WriteByte(digits[r&15])
					} else {
						b.WriteRune(r)
					}
				}
			}
			b.WriteByte('"')
		case float64:
			if v == 0 {
				b.WriteByte('0')
			} else {
				n, _ := json.Marshal(v)
				b.Write(n)
			}
		default:
			raw, _ := json.Marshal(v)
			b.Write(raw)
		}
	}
	emit(value)
	return b.Bytes()
}

func digest(v any) string {
	return hashBytes(canonical(v))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
