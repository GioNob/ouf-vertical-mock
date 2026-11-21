package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type rate struct {
	Count      int           `json:"count"`
	Interval   time.Duration `json:"intervalNanos"`
	Create     int           `json:"create"`
	Update     int           `json:"update"`
	Delete     int           `json:"delete"`
	ReadDelay  time.Duration `json:"readDelayNanos"`
}

func (r rate) valid() bool {
	return r.Count >= 0 && r.Count <= 10000 && r.Interval >= time.Millisecond &&
		r.Create >= 0 && r.Update >= 0 && r.Delete >= 0 && r.Create <= 100 &&
		r.Update <= 100 && r.Delete <= 100 && r.Create+r.Update+r.Delete == 100 && r.ReadDelay >= 0
}

type config struct {
	Port, Dir, RetentionText string
	Retention                time.Duration
	Collections              []string
	Seed                     int64
	Rate                     rate
	ReadToken, AdminToken    [32]byte
}

var retentionPattern = regexp.MustCompile(`^P([0-9]+D)?(T([0-9]+H)?([0-9]+M)?([0-9]+S)?)?$`)

func retention(s string) (time.Duration, error) {
	parts := retentionPattern.FindStringSubmatch(s)
	if parts == nil || strings.HasSuffix(s, "T") {
		return 0, fmt.Errorf("invalid OUF_RETENTION")
	}
	var total time.Duration
	for _, component := range []struct {
		part string
		unit time.Duration
	}{{parts[1], 24*time.Hour}, {parts[3], time.Hour}, {parts[4], time.Minute}, {parts[5], time.Second}} {
		part, unit := component.part, component.unit
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part[:len(part)-1], 10, 64)
		if err != nil || n > (1<<63-1-int64(total))/int64(unit) {
			return 0, fmt.Errorf("invalid OUF_RETENTION")
		}
		total += time.Duration(n) * unit
	}
	if total == 0 {
		return 0, fmt.Errorf("empty retention interval")
	}
	return total, nil
}

func setting(name, fallback string) string {
	if v, ok := os.LookupEnv("OUF_" + name); ok {
		return v
	}
	return fallback
}

func tokenFile(name string) ([32]byte, error) {
	b, err := os.ReadFile(setting(name, ""))
	token := strings.TrimRight(string(b), "\r\n")
	if err != nil || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return [32]byte{}, fmt.Errorf("invalid OUF_%s", name)
	}
	return sha256.Sum256([]byte(token)), nil
}

func loadConfig() (config, error) {
	c := config{Port: setting("PORT", ""), Dir: setting("DATA_DIR", ""), RetentionText: setting("RETENTION", "P7D")}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 || c.Dir == "" {
		return c, fmt.Errorf("set OUF_PORT (1..65535) and OUF_DATA_DIR")
	}
	if c.Retention, err = retention(c.RetentionText); err != nil {
		return c, err
	}
	for _, name := range strings.Split(setting("COLLECTIONS", "edicole,cantieri"), ",") {
		name = strings.TrimSpace(name)
		if (name != "edicole" && name != "cantieri") || slices.Contains(c.Collections, name) {
			return c, fmt.Errorf("invalid OUF_COLLECTIONS")
		}
		c.Collections = append(c.Collections, name)
	}
	if c.Seed, err = strconv.ParseInt(setting("SEED", "1"), 10, 64); err != nil {
		return c, fmt.Errorf("invalid OUF_SEED")
	}
	if c.Rate.Interval, err = time.ParseDuration(setting("INTERVAL", "60s")); err != nil {
		return c, fmt.Errorf("invalid OUF_INTERVAL")
	}
	if c.Rate.ReadDelay, err = time.ParseDuration(setting("READ_DELAY", "0s")); err != nil {
		return c, fmt.Errorf("invalid OUF_READ_DELAY")
	}
	for _, opt := range []struct {
		name, fallback string
		dest           *int
	}{{"COUNT", "5", &c.Rate.Count}, {"CREATE", "50", &c.Rate.Create}, {"UPDATE", "40", &c.Rate.Update}, {"DELETE", "10", &c.Rate.Delete}} {
		if *opt.dest, err = strconv.Atoi(setting(opt.name, opt.fallback)); err != nil {
			return c, fmt.Errorf("invalid OUF_%s", opt.name)
		}
	}
	if !c.Rate.valid() {
		return c, fmt.Errorf("invalid generation rate or proportions")
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
