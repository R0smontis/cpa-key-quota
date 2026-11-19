package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var shanghai, _ = time.LoadLocation("Asia/Shanghai")

func newTestStore(t testing.TB) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), stateFile), shanghai)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

func mustConfig(t testing.TB, s *store, cfg Config) {
	t.Helper()
	if _, err := s.replaceConfig(cfg, -1); err != nil {
		t.Fatal(err)
	}
}

var (
	keyA   = callerScope("sk-alpha")
	keyB   = callerScope("sk-bravo")
	t0     = time.Date(2026, 11, 19, 10, 30, 0, 0, shanghai) // Thursday
	gptUse = Usage{Input: 1000, CacheRead: 9000, Output: 500}
)

func TestCallerScopeMatchesCPA(t *testing.T) {
	// Fixed vector shared with key-model-access, which CPA's interceptor metadata matches.
	if got := callerScope("  sk-test  "); got != callerScope("sk-test") || len(got) != 64 {
		t.Fatalf("scope must trim and be 64 hex chars, got %q", got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		p, n string
		want bool
	}{
		{"*", "anything", true},
		{"gpt-6*", "gpt-6.1-sol", true},
		{"gpt-6*", "gpt-5.6", false},
		{"devin/*", "devin/claude-opus-5-5", true},
		{"*opus*", "devin/claude-opus-5-5", true},
		{"claude-?-5", "claude-x-5", true},
		{"claude-?-5", "claude-xx-5", false},
		{"gpt-6.1-sol", "gpt-6.1-sol", true},
		{"GPT-6*", "gpt-6", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
	}
	for _, c := range cases {
		if got := globMatch(c.p, c.n); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.p, c.n, got, c.want)
		}
	}
}

func TestWindows(t *testing.T) {
	cases := []struct {
		w          Window
		start, end time.Time
	}{
		{WindowHour, time.Date(2026, 11, 19, 10, 0, 0, 0, shanghai), time.Date(2026, 11, 19, 11, 0, 0, 0, shanghai)},
		{WindowDay, time.Date(2026, 11, 19, 0, 0, 0, 0, shanghai), time.Date(2026, 11, 20, 0, 0, 0, 0, shanghai)},
		{WindowWeek, time.Date(2026, 11, 16, 0, 0, 0, 0, shanghai), time.Date(2026, 11, 23, 0, 0, 0, 0, shanghai)},
		{WindowMonth, time.Date(2026, 11, 1, 0, 0, 0, 0, shanghai), time.Date(2026, 12, 1, 0, 0, 0, 0, shanghai)},
	}
	for _, c := range cases {
		s := windowStart(c.w, t0)
		if !s.Equal(c.start) || !windowEnd(c.w, s).Equal(c.end) {
			t.Errorf("%s: got [%v,%v) want [%v,%v)", c.w, s, windowEnd(c.w, s), c.start, c.end)
		}
	}
	// Sunday belongs to the week that started the previous Monday.
	sun := time.Date(2026, 11, 22, 23, 0, 0, 0, shanghai)
	if got := windowStart(WindowWeek, sun); !got.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, shanghai)) {
		t.Errorf("sunday week start = %v", got)
	}
}

// Shapes taken from real CPA usage records on the target host.
func TestNormalizeUsage(t *testing.T) {
	cases := []struct {
		name string
		d    usageDetail
		want Usage
	}{
		{"codex includes cache in input",
			usageDetail{InputTokens: 10000, CachedTokens: 9000, CacheReadTokens: 9000, OutputTokens: 300, ReasoningTokens: 50, TotalTokens: 10300},
			Usage{Input: 1000, CacheRead: 9000, Output: 300}},
		{"claude reports cache separately",
			usageDetail{InputTokens: 7, CachedTokens: 9000, CacheReadTokens: 9000, CacheCreationTokens: 800, OutputTokens: 200, ReasoningTokens: 20, TotalTokens: 10007},
			Usage{Input: 7, CacheRead: 9000, CacheWrite: 800, Output: 200}},
		{"antigravity reasoning outside output",
			usageDetail{InputTokens: 30000, CachedTokens: 20000, CacheReadTokens: 20000, OutputTokens: 400, ReasoningTokens: 1000, TotalTokens: 31400},
			Usage{Input: 10000, CacheRead: 20000, Output: 1400}},
		{"no cache",
			usageDetail{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
			Usage{Input: 100, Output: 50}},
		{"empty", usageDetail{}, Usage{}},
	}
	for _, c := range cases {
		if got := normalizeUsage(c.d); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestPriceSelectionAndCost(t *testing.T) {
	rules := []PriceRule{
		{Pattern: "*", Input: 1, Output: 1},
		{Pattern: "gpt-6*", Input: 2, Output: 8, CacheRead: 0.2},
		{Pattern: "gpt-6.1-sol", Input: 3, Output: 12, CacheRead: 0.3},
	}
	r, ok := priceFor(rules, "gpt-6.1-sol")
	if !ok || r.Pattern != "gpt-6.1-sol" {
		t.Fatalf("exact rule must win, got %+v", r)
	}
	if r, _ := priceFor(rules, "gpt-6-luna"); r.Pattern != "gpt-6*" {
		t.Fatalf("longest glob must win, got %+v", r)
	}
	want := (1000*3 + 9000*0.3 + 500*12) / 1e6
	if got := r.cost(gptUse); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost=%v want %v", got, want)
	}
	if _, ok := priceFor(nil, "x"); ok {
		t.Fatal("no rules must not match")
	}
}

func TestUnmanagedKeyIsUntouched(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {DenyModel: []string{"*"}}}})
	if d := s.admit(keyB, "gpt-6", "", t0); !d.Allowed {
		t.Fatalf("key without policy and no default must pass: %+v", d)
	}
}

func TestModelAllowDeny(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {
		AllowModel: []string{"gpt-6*", "devin/*"},
		DenyModel:  []string{"*-preview"},
	}}})
	for model, want := range map[string]bool{
		"gpt-6.1-sol":           true,
		"devin/claude-opus-5-5": true,
		"gpt-6-preview":         false,
		"claude-opus-5-5":       false,
	} {
		d := s.admit(keyA, model, "", t0)
		if d.Allowed != want {
			t.Errorf("%s: allowed=%v want %v", model, d.Allowed, want)
		}
		if !want && (d.Status != 403 || d.Type != "model_not_allowed") {
			t.Errorf("%s: got %d %s", model, d.Status, d.Type)
		}
	}
}

func TestDisabledKey(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Disabled: true}}})
	if d := s.admit(keyA, "gpt-6", "", t0); d.Allowed || d.Type != "key_disabled" {
		t.Fatalf("got %+v", d)
	}
}

func TestRequestLimitAndRollover(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Limits: []Limit{
		{ID: "daily", Metric: MetricRequests, Window: WindowDay, Max: 2},
	}}}})
	for i := range 2 {
		if d := s.admit(keyA, "m", "", t0); !d.Allowed {
			t.Fatalf("request %d rejected: %+v", i, d)
		}
	}
	d := s.admit(keyA, "m", "", t0)
	if d.Allowed || d.Status != 429 || d.Type != "quota_exceeded" {
		t.Fatalf("third request must be 429, got %+v", d)
	}
	if want := "2026-11-20T00:00:00+08:00"; !strings.Contains(d.Message, want) {
		t.Fatalf("message %q must name reset %s", d.Message, want)
	}
	if d := s.admit(keyA, "m", "", t0.Add(14*time.Hour)); !d.Allowed {
		t.Fatalf("next day must pass: %+v", d)
	}
}

func TestRejectedRequestDoesNotConsume(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Limits: []Limit{
		{ID: "req", Metric: MetricRequests, Window: WindowTotal, Max: 10},
		{ID: "tok", Metric: MetricTokens, Window: WindowTotal, Max: 100},
	}}}})
	s.record(keyA, "m", Usage{Input: 100}, t0)
	if d := s.admit(keyA, "m", "", t0); d.Allowed {
		t.Fatal("token quota exhausted, must reject")
	}
	_, _, views := s.snapshot([]string{keyA})
	if used := views[keyA].Limits[0].Used; used != 0 {
		t.Fatalf("rejected request consumed request quota: %v", used)
	}
}

func TestRetryDedup(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Limits: []Limit{
		{ID: "req", Metric: MetricRequests, Window: WindowTotal, Max: 1},
	}}}})
	for i := range 3 {
		if d := s.admit(keyA, "m", "req-1", t0); !d.Allowed {
			t.Fatalf("retry %d of the same request was charged again", i)
		}
	}
	if d := s.admit(keyA, "m", "req-2", t0); d.Allowed {
		t.Fatal("second distinct request must be rejected")
	}
}

func TestTokenAndCostSettlementScopedByModel(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{
		Pricing: []PriceRule{{Pattern: "gpt-6*", Input: 2, Output: 8, CacheRead: 0.2}},
		Policies: map[string]Policy{keyA: {Limits: []Limit{
			{ID: "gpt-tok", Metric: MetricTokens, Window: WindowMonth, Max: 1e6, Models: []string{"gpt-6*"}},
			{ID: "usd", Metric: MetricCost, Window: WindowMonth, Max: 0.01},
		}}},
	})
	s.record(keyA, "gpt-6.1-sol", gptUse, t0)
	s.record(keyA, "claude-opus-5-5", Usage{Input: 5000, Output: 5000}, t0)
	_, _, v := s.snapshot([]string{keyA})
	byID := map[string]LimitView{}
	for _, l := range v[keyA].Limits {
		byID[l.ID] = l
	}
	if got := byID["gpt-tok"].Used; got != float64(gptUse.Tokens()) {
		t.Errorf("model-scoped token limit counted %v, want %v", got, gptUse.Tokens())
	}
	wantCost := (1000*2 + 9000*0.2 + 500*8) / 1e6 // claude has no price: 0
	if got := byID["usd"].Used; math.Abs(got-wantCost) > 1e-12 {
		t.Errorf("cost=%v want %v", got, wantCost)
	}
	if v[keyA].Totals.Requests != 2 || v[keyA].Totals.Tokens != gptUse.Tokens()+10000 {
		t.Errorf("totals=%+v", v[keyA].Totals)
	}
}

func TestCostLimitBlocksOnceReached(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{
		Pricing:  []PriceRule{{Pattern: "*", Input: 1000}},
		Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "usd", Metric: MetricCost, Window: WindowDay, Max: 1}}}},
	})
	if d := s.admit(keyA, "m", "", t0); !d.Allowed {
		t.Fatal("first request must pass")
	}
	s.record(keyA, "m", Usage{Input: 1000}, t0) // $1.00
	if d := s.admit(keyA, "m", "", t0); d.Allowed {
		t.Fatal("cost quota reached, must reject")
	}
}

func TestDefaultPolicyIsPerKey(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Default: &Policy{Limits: []Limit{{ID: "d", Metric: MetricRequests, Window: WindowDay, Max: 1}}}})
	if !s.admit(keyA, "m", "", t0).Allowed || !s.admit(keyB, "m", "", t0).Allowed {
		t.Fatal("each key must get its own default counter")
	}
	if s.admit(keyA, "m", "", t0).Allowed {
		t.Fatal("key A exhausted its default quota")
	}
}

func TestManualResetAndAdjust(t *testing.T) {
	s := newTestStore(t)
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Limits: []Limit{
		{ID: "a", Metric: MetricRequests, Window: WindowTotal, Max: 1},
		{ID: "b", Metric: MetricTokens, Window: WindowTotal, Max: 100},
	}}}})
	s.admit(keyA, "m", "", t0)
	s.record(keyA, "m", Usage{Input: 100}, t0)
	if s.admit(keyA, "m", "", t0).Allowed {
		t.Fatal("must be exhausted")
	}
	if n, err := s.setUsed(keyA, "a", 0); err != nil || n != 1 {
		t.Fatalf("reset one: n=%d err=%v", n, err)
	}
	if s.admit(keyA, "m", "", t0).Allowed {
		t.Fatal("token limit still exhausted")
	}
	if _, err := s.setUsed(keyA, "", 0); err != nil {
		t.Fatal(err)
	}
	if !s.admit(keyA, "m", "", t0).Allowed {
		t.Fatal("full reset must allow")
	}
	if _, err := s.setUsed(keyA, "b", 42); err != nil {
		t.Fatal(err)
	}
	_, _, v := s.snapshot([]string{keyA})
	if v[keyA].Limits[1].Used != 42 {
		t.Fatalf("adjust failed: %+v", v[keyA].Limits[1])
	}
	if _, err := s.setUsed(keyA, "missing", 0); err == nil {
		t.Fatal("unknown limit must error")
	}
	if _, err := s.setUsed(keyA, "a", -1); err == nil {
		t.Fatal("negative must error")
	}
}

func TestRaisingMaxKeepsUsage(t *testing.T) {
	s := newTestStore(t)
	cfg := Config{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "a", Metric: MetricRequests, Window: WindowTotal, Max: 1}}}}}
	mustConfig(t, s, cfg)
	s.admit(keyA, "m", "", t0)
	cfg.Policies[keyA].Limits[0].Max = 3
	mustConfig(t, s, cfg)
	_, _, v := s.snapshot([]string{keyA})
	if v[keyA].Limits[0].Used != 1 {
		t.Fatal("editing max must not reset usage")
	}
	// Removing the limit drops its counter.
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {}}})
	if _, ok := s.counters[keyA]["a"]; ok {
		t.Fatal("orphan counter kept")
	}
}

func TestRevisionCheck(t *testing.T) {
	s := newTestStore(t)
	rev, err := s.replaceConfig(Config{}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceConfig(Config{}, rev-1); err != errRevision {
		t.Fatalf("stale revision must fail, got %v", err)
	}
	if _, err := s.replaceConfig(Config{}, rev); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	bad := []Config{
		{Policies: map[string]Policy{"not-a-scope": {}}},
		{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "x", Metric: "bytes", Window: WindowDay, Max: 1}}}}},
		{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "x", Metric: MetricCost, Window: "year", Max: 1}}}}},
		{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "x", Metric: MetricCost, Window: WindowDay, Max: -1}}}}},
		{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "x", Metric: MetricCost, Window: WindowDay}, {ID: "x", Metric: MetricCost, Window: WindowDay}}}}},
		{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "bad id!", Metric: MetricCost, Window: WindowDay}}}}},
		{Pricing: []PriceRule{{Pattern: "", Input: 1}}},
		{Pricing: []PriceRule{{Pattern: "*", Input: math.NaN()}}},
	}
	for i, c := range bad {
		if err := c.normalize(); err == nil {
			t.Errorf("case %d must fail validation", i)
		}
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFile)
	s, err := openStore(path, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	mustConfig(t, s, Config{Policies: map[string]Policy{keyA: {Limits: []Limit{{ID: "a", Metric: MetricRequests, Window: WindowTotal, Max: 5}}}}})
	s.admit(keyA, "m", "", t0)
	s.admit(keyA, "m", "", t0)
	s.close() // flushes
	s2, err := openStore(path, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	_, _, v := s2.snapshot([]string{keyA})
	if v[keyA].Limits[0].Used != 2 {
		t.Fatalf("usage lost across restart: %+v", v[keyA])
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, want 0600", info.Mode().Perm())
	}
}

func TestCorruptStateRefusesToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFile)
	os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := openStore(path, shanghai); err == nil {
		t.Fatal("corrupt state must not be silently replaced")
	}
}

// End to end through the RPC surface the host calls.
func TestRPCFlow(t *testing.T) {
	dir := t.TempDir()
	cfgYAML := fmt.Sprintf("data_dir: %s\ntimezone: Asia/Shanghai\n", dir)
	reg, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfgYAML), SchemaVersion: 6})
	out, err := handleMethod("plugin.register", reg)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Metadata     map[string]any  `json:"metadata"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	json.Unmarshal(out, &env)
	if !env.OK || !env.Result.Capabilities["request_interceptor"] || !env.Result.Capabilities["usage_plugin"] {
		t.Fatalf("registration: %s", out)
	}
	// CPA's pluginhost.validPlugin rejects a registration unless these are non-empty.
	for _, f := range []string{"Name", "Version", "Author", "GitHubRepository"} {
		if v, _ := env.Result.Metadata[f].(string); v == "" {
			t.Fatalf("metadata %s must be non-empty for the host to accept the plugin", f)
		}
	}
	t.Cleanup(global.shutdown)

	cfg, _ := json.Marshal(Config{
		Pricing:  []PriceRule{{Pattern: "*", Input: 1, Output: 1}},
		Policies: map[string]Policy{keyA: {AllowModel: []string{"gpt-*"}, Limits: []Limit{{ID: "t", Metric: MetricTokens, Window: WindowDay, Max: 100}}}},
	})
	mgmt := func(method, path string, body []byte) (int, map[string]any) {
		raw, _ := json.Marshal(managementRequest{Method: method, Path: path, Body: body})
		out, err := handleMethod("management.handle", raw)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Result managementResponse `json:"result"`
		}
		json.Unmarshal(out, &e)
		var m map[string]any
		json.Unmarshal(e.Result.Body, &m)
		return e.Result.StatusCode, m
	}
	if code, m := mgmt("PUT", policyPath, cfg); code != 200 {
		t.Fatalf("put config: %d %v", code, m)
	}

	intercept := func(model string) map[string]any {
		// v8.0.67 sends the scope both top-level and in Metadata; exercise the top-level form.
		raw := fmt.Sprintf(`{"RequestID":"%d","RequestedModel":%q,"CallerScope":%q,"Metadata":{}}`, time.Now().UnixNano(), model, keyA)
		out, err := handleMethod("request.intercept_before", []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Result map[string]any `json:"result"`
		}
		json.Unmarshal(out, &e)
		return e.Result
	}
	if r := intercept("gpt-6"); r["Terminate"] == true {
		t.Fatalf("allowed model terminated: %v", r)
	}
	if r := intercept("claude-x"); r["Terminate"] != true || r["StatusCode"].(float64) != 403 {
		t.Fatalf("denied model: %v", r)
	}
	usage := `{"Model":"gpt-6","Alias":"gpt-6","APIKey":"sk-alpha","Detail":{"InputTokens":80,"OutputTokens":20,"TotalTokens":100}}`
	if _, err := handleMethod("usage.handle", []byte(usage)); err != nil {
		t.Fatal(err)
	}
	if r := intercept("gpt-6"); r["Terminate"] != true || r["StatusCode"].(float64) != 429 {
		t.Fatalf("token quota must block: %v", r)
	}
	if code, _ := mgmt("POST", usagePath, []byte(fmt.Sprintf(`{"scope":%q}`, keyA))); code != 200 {
		t.Fatal("reset failed")
	}
	if r := intercept("gpt-6"); r["Terminate"] == true {
		t.Fatalf("after reset must pass: %v", r)
	}
	code, st := mgmt("POST", statePath, []byte(fmt.Sprintf(`{"scopes":[%q]}`, keyB)))
	keys := st["keys"].(map[string]any)
	if code != 200 || keys[keyA] == nil || keys[keyB].(map[string]any)["source"] != "none" {
		t.Fatalf("state: %d %v", code, st)
	}
	// Requests without a CPA caller scope are not ours to police.
	out, _ = handleMethod("request.intercept_before", []byte(`{"RequestedModel":"claude-x","Metadata":{}}`))
	if string(out) != string(okNoop) {
		t.Fatalf("unauthenticated request must pass through: %s", out)
	}
}

// CPA v8.0.67 serves these under /v0/management itself (internal/api/server_management.go).
// A plugin route with the same method and path never reaches the plugin: PUT
// /plugins/:id/config, for one, rewrites the plugin's config.yaml block and
// hot-reloads (unloads) it.
func TestRoutesAvoidHostBuiltins(t *testing.T) {
	builtin := map[string]bool{
		"DELETE /plugins/" + pluginID:                true,
		"PATCH /plugins/" + pluginID + "/enabled":    true,
		"GET /plugins/" + pluginID + "/config":       true,
		"PUT /plugins/" + pluginID + "/config":       true,
		"PATCH /plugins/" + pluginID + "/config":     true,
		"GET /plugins/" + pluginID + "/quota":        true,
		"POST /plugins/" + pluginID + "/quota":       true,
		"DELETE /plugins/" + pluginID + "/quota":     true,
		"POST /plugins/" + pluginID + "/quota/reset": true,
	}
	for _, r := range managementRegistration()["routes"].([]route) {
		key := r.Method + " " + strings.TrimPrefix(r.Path, "/v0/management")
		if builtin[key] {
			t.Errorf("route %s is shadowed by a CPA built-in management route", key)
		}
	}
}

// ---- benchmarks: the request path must stay far below network latency ----

func benchStore(b *testing.B) *store {
	s := newTestStore(b)
	policies := map[string]Policy{}
	for i := range 200 {
		policies[callerScope(fmt.Sprint("k", i))] = Policy{
			AllowModel: []string{"gpt-6*", "claude-*", "devin/*"},
			DenyModel:  []string{"*-preview"},
			Limits: []Limit{
				{ID: "r", Metric: MetricRequests, Window: WindowDay, Max: 1e12},
				{ID: "t", Metric: MetricTokens, Window: WindowMonth, Max: 1e15},
				{ID: "c", Metric: MetricCost, Window: WindowMonth, Max: 1e12, Models: []string{"claude-*"}},
			},
		}
	}
	mustConfig(b, s, Config{Policies: policies, Pricing: []PriceRule{{Pattern: "*", Input: 1, Output: 1}}})
	return s
}

func BenchmarkAdmit(b *testing.B) {
	s := benchStore(b)
	scope := callerScope("k7")
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		s.admit(scope, "claude-opus-5-5", "", now)
	}
}

func BenchmarkInterceptRPC(b *testing.B) {
	s := benchStore(b)
	global.s.Store(s)
	b.Cleanup(func() { global.s.Store(nil) })
	raw := []byte(fmt.Sprintf(`{"RequestID":"","SourceFormat":"openai","Model":"claude-opus-5-5","RequestedModel":"claude-opus-5-5","Stream":true,"Headers":{"Content-Type":["application/json"]},"Metadata":{"caller_scope":%q,"source":"x"}}`, callerScope("k7")))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := handleMethod("request.intercept_before", raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUsageRPC(b *testing.B) {
	s := benchStore(b)
	global.s.Store(s)
	b.Cleanup(func() { global.s.Store(nil) })
	raw := []byte(`{"Provider":"claude","Model":"claude-opus-5-5","Alias":"claude-opus-5-5","APIKey":"k7","RequestedAt":"2026-11-19T10:00:00Z","Latency":1200000000,"Detail":{"InputTokens":7,"CachedTokens":9000,"CacheReadTokens":9000,"CacheCreationTokens":800,"OutputTokens":200,"TotalTokens":10007}}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := handleMethod("usage.handle", raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdmitParallel(b *testing.B) {
	s := benchStore(b)
	now := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.admit(callerScope(fmt.Sprint("k", i%200)), "gpt-6.1-sol", "", now)
			i++
		}
	})
}
