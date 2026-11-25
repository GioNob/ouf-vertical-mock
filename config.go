package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type config struct {
	Port, Dir, DomainFile     string
	Seed                      int64
	Cycle, Retention, MaxLate time.Duration
	ReadInterval              time.Duration
	ClockControl              bool
	ReadToken, AdminToken     [32]byte
}

var durationPattern = regexp.MustCompile(`^P([0-9]+D)?(T([0-9]+H)?([0-9]+M)?([0-9]+(\.[0-9]{1,9})?S)?)?$`)

func duration(s string) (time.Duration, error) {
	parts := durationPattern.FindStringSubmatch(s)
	if parts == nil || s == "P" || strings.HasSuffix(s, "T") {
		return 0, fmt.Errorf("expected ISO 8601 duration")
	}
	var total time.Duration
	for _, component := range []struct {
		part string
		unit time.Duration
	}{{parts[1], 24 * time.Hour}, {parts[3], time.Hour}, {parts[4], time.Minute}, {parts[5], time.Second}} {
		part, unit := component.part, component.unit
		if part == "" {
			continue
		}
		integer, fraction, _ := strings.Cut(part[:len(part)-1], ".")
		n, err := strconv.ParseInt(integer, 10, 64)
		if err != nil || n > int64((1<<63-1-total)/unit) {
			return 0, fmt.Errorf("duration exceeds supported range")
		}
		total += time.Duration(n) * unit
		if fraction != "" {
			ns, _ := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
			if ns > 1<<63-1-int64(total) {
				return 0, fmt.Errorf("duration exceeds supported range")
			}
			total += time.Duration(ns)
		}
	}
	return total, nil
}

func iso(d time.Duration) string {
	if d == 0 {
		return "PT0S"
	}
	var b strings.Builder
	b.WriteByte('P')
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dD", days)
		d %= 24 * time.Hour
	}
	if d > 0 {
		b.WriteByte('T')
		if h := d / time.Hour; h > 0 {
			fmt.Fprintf(&b, "%dH", h)
			d %= time.Hour
		}
		if m := d / time.Minute; m > 0 {
			fmt.Fprintf(&b, "%dM", m)
			d %= time.Minute
		}
		if d > 0 {
			fmt.Fprintf(&b, "%d", d/time.Second)
			if ns := d % time.Second; ns != 0 {
				b.WriteString(strings.TrimRight(fmt.Sprintf(".%09d", ns), "0"))
			}
			b.WriteByte('S')
		}
	}
	return b.String()
}

func setting(name, fallback string) string {
	if v, ok := os.LookupEnv("OUF_MOCK_" + name); ok {
		return v
	}
	return fallback
}

func tokenFile(name string) ([32]byte, error) {
	b, err := os.ReadFile(setting(name, ""))
	token := strings.TrimRight(string(b), "\r\n")
	if err != nil || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return [32]byte{}, fmt.Errorf("invalid OUF_MOCK_%s", name)
	}
	return sha256.Sum256([]byte(token)), nil
}

func loadConfig() (config, error) {
	c := config{Port: setting("PORT", "8080"), Dir: setting("DATA_DIR", ""), DomainFile: setting("DOMAIN_FILE", "")}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return c, fmt.Errorf("OUF_MOCK_PORT must be in 1..65535")
	}
	if c.Dir == "" || c.DomainFile == "" {
		return c, fmt.Errorf("OUF_MOCK_DATA_DIR and OUF_MOCK_DOMAIN_FILE are required")
	}
	if c.Seed, err = strconv.ParseInt(setting("SEED", "1"), 10, 64); err != nil {
		return c, fmt.Errorf("invalid OUF_MOCK_SEED")
	}
	for _, opt := range []struct {
		name, fallback string
		dest           *time.Duration
		min            time.Duration
	}{
		{"CYCLE", "PT1M", &c.Cycle, time.Second},
		{"MAX_LATENESS", "PT10M", &c.MaxLate, 0},
		{"RETENTION", "P7D", &c.Retention, time.Nanosecond},
		{"READ_MIN_INTERVAL", "PT0S", &c.ReadInterval, 0},
	} {
		*opt.dest, err = duration(setting(opt.name, opt.fallback))
		if err != nil || *opt.dest < opt.min {
			return c, fmt.Errorf("invalid OUF_MOCK_%s", opt.name)
		}
	}
	if c.MaxLate >= c.Retention {
		return c, fmt.Errorf("OUF_MOCK_MAX_LATENESS must be shorter than OUF_MOCK_RETENTION")
	}
	switch setting("CLOCK_CONTROL", "disabled") {
	case "enabled":
		c.ClockControl = true
	case "disabled":
	default:
		return c, fmt.Errorf("invalid OUF_MOCK_CLOCK_CONTROL")
	}
	if c.ReadToken, err = tokenFile("READ_TOKEN_FILE"); err != nil {
		return c, err
	}
	if c.AdminToken, err = tokenFile("ADMIN_TOKEN_FILE"); err != nil {
		return c, err
	}
	if c.ReadToken == c.AdminToken {
		return c, fmt.Errorf("read and admin credentials must differ")
	}
	return c, nil
}
