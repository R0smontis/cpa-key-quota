package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Metric is what a limit counts.
type Metric string

const (
	MetricRequests Metric = "requests"
	MetricTokens   Metric = "tokens"
	MetricCost     Metric = "cost" // USD
)

// Window is the period after which a limit's counter starts over.
type Window string

const (
	WindowTotal Window = "total" // never resets on its own
	WindowHour  Window = "hour"
	WindowDay   Window = "day"
	WindowWeek  Window = "week" // weeks start on Monday
	WindowMonth Window = "month"
)

// Limit caps one metric over one window, optionally only for some models.
type Limit struct {
	ID     string   `json:"id"`
	Metric Metric   `json:"metric"`
	Window Window   `json:"window"`
	Max    float64  `json:"max"`
	Models []string `json:"models,omitempty"` // empty: every model
}

// Policy is the rule set of one API key (or of the default policy).
type Policy struct {
	Label      string   `json:"label,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`
	AllowModel []string `json:"allow_models,omitempty"` // empty: every model not denied
	DenyModel  []string `json:"deny_models,omitempty"`
	Limits     []Limit  `json:"limits,omitempty"`
}

// PriceRule prices one model pattern in USD per million tokens.
type PriceRule struct {
	Pattern    string  `json:"pattern"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Config is the operator-edited part of the state.
type Config struct {
	// Default applies to every key that has no policy of its own. Each key still
	// gets its own counters, so a default "100 requests per day" means 100 per key.
	Default  *Policy           `json:"default,omitempty"`
	Policies map[string]Policy `json:"policies"` // keyed by CPA caller scope
	Pricing  []PriceRule       `json:"pricing"`
}

var (
	scopeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// callerScope mirrors CPA's built-in API-key identity derivation, the same
// value CPA passes to interceptors as Metadata.caller_scope.
func callerScope(rawKey string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(rawKey)))
	return hex.EncodeToString(sum[:])
}

func (c *Config) normalize() error {
	if c.Policies == nil {
		c.Policies = map[string]Policy{}
	}
	if c.Default != nil {
		p, err := normalizePolicy(*c.Default, "default")
		if err != nil {
			return err
		}
		c.Default = &p
	}
	fixed := make(map[string]Policy, len(c.Policies))
	for scope, p := range c.Policies {
		s := strings.ToLower(strings.TrimSpace(scope))
		if !scopeRE.MatchString(s) {
			return fmt.Errorf("invalid caller scope %q", scope)
		}
		if _, dup := fixed[s]; dup {
			return fmt.Errorf("duplicate caller scope %s", s)
		}
		np, err := normalizePolicy(p, s[:10])
		if err != nil {
			return err
		}
		fixed[s] = np
	}
	c.Policies = fixed
	for i := range c.Pricing {
		r := &c.Pricing[i]
		r.Pattern = strings.TrimSpace(r.Pattern)
		if r.Pattern == "" {
			return fmt.Errorf("pricing[%d]: pattern is required", i)
		}
		for _, v := range []float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("pricing[%d]: prices must be finite and non-negative", i)
			}
		}
	}
	return nil
}

func normalizePolicy(p Policy, where string) (Policy, error) {
	p.Label = strings.TrimSpace(p.Label)
	if len(p.Label) > 80 {
		return p, fmt.Errorf("%s: label too long", where)
	}
	p.AllowModel = cleanPatterns(p.AllowModel)
	p.DenyModel = cleanPatterns(p.DenyModel)
	seen := map[string]bool{}
	for i := range p.Limits {
		l := &p.Limits[i]
		if !idRE.MatchString(l.ID) {
			return p, fmt.Errorf("%s: limit %d has invalid id", where, i)
		}
		if seen[l.ID] {
			return p, fmt.Errorf("%s: duplicate limit id %s", where, l.ID)
		}
		seen[l.ID] = true
		switch l.Metric {
		case MetricRequests, MetricTokens, MetricCost:
		default:
			return p, fmt.Errorf("%s: limit %s has unknown metric %q", where, l.ID, l.Metric)
		}
		switch l.Window {
		case WindowTotal, WindowHour, WindowDay, WindowWeek, WindowMonth:
		default:
			return p, fmt.Errorf("%s: limit %s has unknown window %q", where, l.ID, l.Window)
		}
		if l.Max < 0 || math.IsNaN(l.Max) || math.IsInf(l.Max, 0) {
			return p, fmt.Errorf("%s: limit %s max must be finite and non-negative", where, l.ID)
		}
		l.Models = cleanPatterns(l.Models)
	}
	return p, nil
}

func cleanPatterns(in []string) []string {
	out := in[:0]
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// globMatch reports whether name matches pattern, where '*' matches any run of
// characters (including '/') and '?' matches exactly one. Case-sensitive.
func globMatch(pattern, name string) bool {
	p, n := 0, 0
	star, mark := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case star >= 0:
			p = star + 1
			mark++
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func (p *Policy) allowsModel(model string) bool {
	if matchAny(p.DenyModel, model) {
		return false
	}
	return len(p.AllowModel) == 0 || matchAny(p.AllowModel, model)
}

func (l *Limit) appliesTo(model string) bool {
	return len(l.Models) == 0 || matchAny(l.Models, model)
}

// windowStart returns the start of the window containing t.
func windowStart(w Window, t time.Time) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch w {
	case WindowHour:
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	case WindowDay:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	case WindowWeek:
		offset := (int(t.Weekday()) + 6) % 7 // Monday = 0
		return time.Date(y, m, d-offset, 0, 0, 0, 0, loc)
	case WindowMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	default:
		return time.Time{}
	}
}

// windowEnd returns when the window starting at start ends (zero for total).
func windowEnd(w Window, start time.Time) time.Time {
	switch w {
	case WindowHour:
		return start.Add(time.Hour)
	case WindowDay:
		return start.AddDate(0, 0, 1)
	case WindowWeek:
		return start.AddDate(0, 0, 7)
	case WindowMonth:
		return start.AddDate(0, 1, 0)
	default:
		return time.Time{}
	}
}

// Usage is the normalized accounting of one finished request.
type Usage struct {
	Input      int64 // input tokens excluding cache reads and writes
	CacheRead  int64
	CacheWrite int64
	Output     int64 // output tokens including reasoning
}

func (u Usage) Tokens() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.Output }

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// normalizeUsage reconciles provider conventions: OpenAI-style counts report
// cached tokens inside InputTokens, Anthropic-style counts report them
// separately, and some providers leave reasoning out of OutputTokens. The
// reported total tells them apart.
func normalizeUsage(d usageDetail) Usage {
	cacheRead := max(d.CacheReadTokens, d.CachedTokens, 0)
	cacheWrite := max(d.CacheCreationTokens, 0)
	input := max(d.InputTokens, 0)
	output := max(d.OutputTokens, 0)
	reasoning := max(d.ReasoningTokens, 0)
	cache := cacheRead + cacheWrite

	separate := input + output + cache // Anthropic-style sum
	includesCache := cache > 0 && input >= cache && !(d.TotalTokens > 0 && d.TotalTokens >= separate)
	if includesCache {
		input -= cache
	}
	if reasoning > 0 && d.TotalTokens >= input+cache+output+reasoning {
		output += reasoning
	}
	return Usage{Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite, Output: output}
}

func priceFor(rules []PriceRule, model string) (PriceRule, bool) {
	// Exact patterns win over globs; among globs the longest pattern wins.
	best, bestScore := PriceRule{}, -1
	for _, r := range rules {
		if !globMatch(r.Pattern, model) {
			continue
		}
		score := len(r.Pattern)
		if !strings.ContainsAny(r.Pattern, "*?") {
			score += 1 << 20
		}
		if score > bestScore {
			best, bestScore = r, score
		}
	}
	return best, bestScore >= 0
}

func (r PriceRule) cost(u Usage) float64 {
	return (float64(u.Input)*r.Input + float64(u.Output)*r.Output +
		float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*r.CacheWrite) / 1e6
}
