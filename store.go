package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // windows must work even if the container has no zoneinfo
)

const (
	stateFile     = "key-quota.json"
	flushInterval = 2 * time.Second
	dedupSize     = 4096
)

// counter is the running value of one limit of one key.
type counter struct {
	Window Window  `json:"window"`
	Start  int64   `json:"start"` // unix seconds of the window start; 0 for total
	Used   float64 `json:"used"`
}

// totals is lifetime usage of one key, kept for the console.
type totals struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	LastUsed int64   `json:"last_used,omitempty"`
}

type persisted struct {
	Version  int                            `json:"version"`
	Revision int64                          `json:"revision"`
	Config   Config                         `json:"config"`
	Counters map[string]map[string]*counter `json:"counters"` // scope -> limit id
	Totals   map[string]*totals             `json:"totals"`
}

// compiled is an immutable view of the config used on the request path. It
// holds policies by pointer so lookups never copy a Policy.
type compiled struct {
	cfg      Config
	revision int64
	byScope  map[string]*Policy
}

func compile(cfg Config, revision int64) *compiled {
	c := &compiled{cfg: cfg, revision: revision, byScope: make(map[string]*Policy, len(cfg.Policies))}
	for scope, p := range cfg.Policies {
		c.byScope[scope] = &p
	}
	return c
}

func (c *compiled) policyFor(scope string) (*Policy, bool) {
	if p, ok := c.byScope[scope]; ok {
		return p, true
	}
	if c.cfg.Default != nil {
		return c.cfg.Default, false
	}
	return nil, false
}

type store struct {
	cfg atomic.Pointer[compiled]

	mu       sync.Mutex
	counters map[string]map[string]*counter
	totals   map[string]*totals
	dirty    bool
	path     string
	loc      *time.Location
	revision int64

	seen     map[string]struct{}
	seenRing [dedupSize]string
	seenPos  int

	stop chan struct{}
	done chan struct{}
}

var global = &holder{}

// holder lets reconfigure swap the store without racing in-flight calls.
type holder struct {
	mu sync.Mutex
	s  atomic.Pointer[store]
}

func (h *holder) get() *store { return h.s.Load() }

func (h *holder) open(cfg pluginConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", cfg.Timezone, err)
	}
	path := filepath.Join(cfg.DataDir, stateFile)
	if cur := h.s.Load(); cur != nil && cur.path == path {
		cur.mu.Lock()
		cur.loc = loc
		cur.mu.Unlock()
		return nil
	}
	s, err := openStore(path, loc)
	if err != nil {
		return err
	}
	if old := h.s.Swap(s); old != nil {
		old.close()
	}
	return nil
}

func (h *holder) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.s.Swap(nil); old != nil {
		old.close()
	}
}

func openStore(path string, loc *time.Location) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	p := persisted{Version: 1}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	default:
		if err := json.Unmarshal(raw, &p); err != nil {
			// Refuse to start over a corrupt file: silently resetting would wipe limits.
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := p.Config.normalize(); err != nil {
		return nil, fmt.Errorf("invalid config in %s: %w", path, err)
	}
	s := &store{
		counters: p.Counters,
		totals:   p.Totals,
		path:     path,
		loc:      loc,
		revision: p.Revision,
		seen:     make(map[string]struct{}, dedupSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if s.counters == nil {
		s.counters = map[string]map[string]*counter{}
	}
	if s.totals == nil {
		s.totals = map[string]*totals{}
	}
	s.cfg.Store(compile(p.Config, p.Revision))
	if errors.Is(err, os.ErrNotExist) {
		if err := s.writeLocked(); err != nil {
			return nil, err
		}
	}
	go s.flusher()
	return s, nil
}

func (s *store) close() {
	close(s.stop)
	<-s.done
}

func (s *store) flusher() {
	defer close(s.done)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stop:
			s.flush()
			return
		}
	}
}

func (s *store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	if err := s.writeLocked(); err != nil {
		log.Printf("key-quota: persist state: %v", err)
		return
	}
	s.dirty = false
}

// writeLocked persists everything atomically. Caller holds s.mu.
func (s *store) writeLocked() error {
	raw, err := json.Marshal(persisted{
		Version:  1,
		Revision: s.revision,
		Config:   s.cfg.Load().cfg,
		Counters: s.counters,
		Totals:   s.totals,
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".key-quota-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// current returns the counter for the limit, rolled to the window containing
// now. Caller holds s.mu.
func (s *store) current(scope string, l *Limit, now time.Time) *counter {
	start := windowStart(l.Window, now.In(s.loc))
	var startUnix int64
	if !start.IsZero() {
		startUnix = start.Unix()
	}
	byLimit := s.counters[scope]
	if byLimit == nil {
		byLimit = map[string]*counter{}
		s.counters[scope] = byLimit
	}
	c := byLimit[l.ID]
	if c == nil {
		c = &counter{Window: l.Window, Start: startUnix}
		byLimit[l.ID] = c
		return c
	}
	if c.Window != l.Window || c.Start != startUnix {
		c.Window, c.Start, c.Used = l.Window, startUnix, 0
		s.dirty = true
	}
	return c
}

// Decision is the outcome of admitting a request.
type Decision struct {
	Allowed bool
	Status  int
	Type    string
	Message string
}

var allow = Decision{Allowed: true}

// admit checks the key's policy and, when allowed, counts the request against
// its request limits. requestID de-duplicates host retries of one request.
func (s *store) admit(scope, model, requestID string, now time.Time) Decision {
	cfg := s.cfg.Load()
	p, _ := cfg.policyFor(scope)
	if p == nil {
		return allow
	}
	if p.Disabled {
		return Decision{Status: 403, Type: "key_disabled", Message: "this API key is disabled"}
	}
	if !p.allowsModel(model) {
		return Decision{Status: 403, Type: "model_not_allowed", Message: "model is not allowed for this API key"}
	}
	if len(p.Limits) == 0 {
		return allow
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if requestID != "" {
		if _, dup := s.seen[requestID]; dup {
			return allow
		}
	}
	// Check every applicable limit before counting, so a rejected request
	// never consumes request quota.
	for i := range p.Limits {
		l := &p.Limits[i]
		if !l.appliesTo(model) {
			continue
		}
		c := s.current(scope, l, now)
		need := 0.0
		if l.Metric == MetricRequests {
			need = 1
		}
		if c.Used+need > l.Max || (need == 0 && c.Used >= l.Max) {
			return Decision{Status: 429, Type: "quota_exceeded", Message: quotaMessage(l, s.resetAt(l, c))}
		}
	}
	for i := range p.Limits {
		l := &p.Limits[i]
		if l.Metric == MetricRequests && l.appliesTo(model) {
			s.current(scope, l, now).Used++
			s.dirty = true
		}
	}
	if requestID != "" {
		s.remember(requestID)
	}
	return allow
}

func (s *store) remember(id string) {
	if old := s.seenRing[s.seenPos]; old != "" {
		delete(s.seen, old)
	}
	s.seenRing[s.seenPos] = id
	s.seen[id] = struct{}{}
	s.seenPos = (s.seenPos + 1) % dedupSize
}

func (s *store) resetAt(l *Limit, c *counter) time.Time {
	if l.Window == WindowTotal {
		return time.Time{}
	}
	return windowEnd(l.Window, time.Unix(c.Start, 0).In(s.loc))
}

func quotaMessage(l *Limit, reset time.Time) string {
	unit := map[Metric]string{MetricRequests: "requests", MetricTokens: "tokens", MetricCost: "USD"}[l.Metric]
	msg := fmt.Sprintf("quota %q exceeded: %s %s per %s", l.ID, trimFloat(l.Max), unit, l.Window)
	if !reset.IsZero() {
		msg += ", resets at " + reset.Format(time.RFC3339)
	}
	return msg
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// record settles a finished request's tokens and cost.
func (s *store) record(scope, model string, u Usage, at time.Time) {
	cfg := s.cfg.Load()
	tokens := u.Tokens()
	cost := 0.0
	if rule, ok := priceFor(cfg.cfg.Pricing, model); ok {
		cost = rule.cost(u)
	}
	p, _ := cfg.policyFor(scope)

	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.totals[scope]
	if t == nil {
		t = &totals{}
		s.totals[scope] = t
	}
	t.Requests++
	t.Tokens += tokens
	t.Cost += cost
	t.LastUsed = at.Unix()
	s.dirty = true
	if p == nil {
		return
	}
	for i := range p.Limits {
		l := &p.Limits[i]
		if !l.appliesTo(model) {
			continue
		}
		switch l.Metric {
		case MetricTokens:
			s.current(scope, l, at).Used += float64(tokens)
		case MetricCost:
			s.current(scope, l, at).Used += cost
		}
	}
}

var errRevision = errors.New("revision mismatch")

// replaceConfig swaps the operator config. Counters of limits that no longer
// exist are dropped; counters of kept limits survive edits to their max.
func (s *store) replaceConfig(cfg Config, ifRevision int64) (int64, error) {
	if err := cfg.normalize(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ifRevision >= 0 && ifRevision != s.revision {
		return 0, errRevision
	}
	for scope, byLimit := range s.counters {
		p, own := cfg.Policies[scope]
		var active *Policy
		switch {
		case own:
			active = &p
		case cfg.Default != nil:
			active = cfg.Default
		}
		for id := range byLimit {
			if active == nil || !hasLimit(active, id) {
				delete(byLimit, id)
			}
		}
		if len(byLimit) == 0 {
			delete(s.counters, scope)
		}
	}
	s.revision++
	s.cfg.Store(compile(cfg, s.revision))
	if err := s.writeLocked(); err != nil {
		return 0, err
	}
	s.dirty = false
	return s.revision, nil
}

func hasLimit(p *Policy, id string) bool {
	for _, l := range p.Limits {
		if l.ID == id {
			return true
		}
	}
	return false
}

// setUsed sets (or with used=0, resets) counters. Empty limitID targets every
// limit of the scope; scope "*" targets every key.
func (s *store) setUsed(scope, limitID string, used float64) (int, error) {
	if used < 0 {
		return 0, fmt.Errorf("used must be non-negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg.Load()
	now := time.Now()
	n := 0
	if scope == "*" {
		if used != 0 || limitID != "" {
			return 0, fmt.Errorf("only a full reset may target every key")
		}
		for _, byLimit := range s.counters {
			for _, c := range byLimit {
				c.Used = 0
				n++
			}
		}
	} else {
		p, _ := cfg.policyFor(scope)
		if p == nil {
			return 0, fmt.Errorf("key has no limits")
		}
		for i := range p.Limits {
			l := &p.Limits[i]
			if limitID != "" && l.ID != limitID {
				continue
			}
			s.current(scope, l, now).Used = used
			n++
		}
		if n == 0 {
			return 0, fmt.Errorf("limit %q not found", limitID)
		}
	}
	if err := s.writeLocked(); err != nil {
		return 0, err
	}
	s.dirty = false
	return n, nil
}

// LimitView is one limit with its live counter, for the console.
type LimitView struct {
	Limit
	Used     float64 `json:"used"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

type KeyView struct {
	Source string      `json:"source"` // "own" | "default" | "none"
	Limits []LimitView `json:"limits"`
	Totals totals      `json:"totals"`
}

func (s *store) snapshot(scopes []string) (Config, int64, map[string]KeyView) {
	cfg := s.cfg.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make(map[string]KeyView, len(scopes))
	for _, scope := range scopes {
		p, own := cfg.policyFor(scope)
		v := KeyView{Source: "none", Limits: []LimitView{}}
		if t := s.totals[scope]; t != nil {
			v.Totals = *t
		}
		if p != nil {
			v.Source = "default"
			if own {
				v.Source = "own"
			}
			for i := range p.Limits {
				l := &p.Limits[i]
				c := s.current(scope, l, now)
				lv := LimitView{Limit: *l, Used: c.Used}
				if r := s.resetAt(l, c); !r.IsZero() {
					lv.ResetsAt = r.Format(time.RFC3339)
				}
				v.Limits = append(v.Limits, lv)
			}
		}
		out[scope] = v
	}
	return cfg.cfg, cfg.revision, out
}
