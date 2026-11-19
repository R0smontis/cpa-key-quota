package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	basePath  = "/v0/management/plugins/" + pluginID
	statePath = basePath + "/state"
	// Not "/config": CPA owns PUT /v0/management/plugins/:id/config and would
	// write the body into the plugin's config.yaml block instead of calling us.
	policyPath   = basePath + "/policy"
	usagePath    = basePath + "/usage"
	consolePath  = "/v0/resource/plugins/" + pluginID + "/console"
	maxBodyBytes = 1 << 20
)

// interceptRequest decodes only what admission needs; the request body is
// omitted by the host (request_interceptor_omit_body_before).
type interceptRequest struct {
	RequestID      string `json:"RequestID"`
	Model          string `json:"Model"`
	RequestedModel string `json:"RequestedModel"`
	CallerScope    string `json:"CallerScope"`
	Metadata       struct {
		CallerScope string `json:"caller_scope"`
	} `json:"Metadata"`
}

func interceptBefore(raw []byte) ([]byte, error) {
	s := global.get()
	if s == nil {
		return okNoop, nil
	}
	var req interceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	scope := req.CallerScope
	if scope == "" {
		scope = req.Metadata.CallerScope
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	if !scopeRE.MatchString(scope) {
		// Not authenticated by a CPA api-key (or no auth at all): not ours to police.
		return okNoop, nil
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	d := s.admit(scope, model, req.RequestID, time.Now())
	if d.Allowed {
		return okNoop, nil
	}
	return terminate(d.Status, d.Type, d.Message, model)
}

type usageRecord struct {
	Model       string      `json:"Model"`
	Alias       string      `json:"Alias"`
	APIKey      string      `json:"APIKey"`
	RequestedAt time.Time   `json:"RequestedAt"`
	Detail      usageDetail `json:"Detail"`
}

// scopeCache avoids re-hashing the same raw key on every usage event. It is
// bounded: CPA keys are few, and an overflow just clears and starts over.
var (
	scopeMu    sync.RWMutex
	scopeCache = map[string]string{}
)

const scopeCacheMax = 4096

func scopeOf(rawKey string) string {
	scopeMu.RLock()
	s, ok := scopeCache[rawKey]
	scopeMu.RUnlock()
	if ok {
		return s
	}
	s = callerScope(rawKey)
	scopeMu.Lock()
	if len(scopeCache) >= scopeCacheMax {
		clear(scopeCache)
	}
	scopeCache[rawKey] = s
	scopeMu.Unlock()
	return s
}

func handleUsage(raw []byte) ([]byte, error) {
	s := global.get()
	if s == nil || len(raw) == 0 {
		return okNoop, nil
	}
	var rec usageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return okNoop, nil
	}
	key := strings.TrimSpace(rec.APIKey)
	if key == "" {
		return okNoop, nil
	}
	// Limits match the model name the client asked for, like admission does.
	model := strings.TrimSpace(rec.Alias)
	if model == "" {
		model = strings.TrimSpace(rec.Model)
	}
	at := rec.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	s.record(scopeOf(key), model, normalizeUsage(rec.Detail), at)
	return okNoop, nil
}

type route struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description,omitempty"`
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []route{
			{Method: http.MethodPost, Path: statePath, Description: "Read config and live counters for the given caller scopes."},
			{Method: http.MethodPut, Path: policyPath, Description: "Replace policies, default policy and pricing."},
			{Method: http.MethodPost, Path: usagePath, Description: "Reset or set a key's limit counters."},
		},
		"resources": []resource{{Path: "/console", Menu: "Key 配额", Description: "Per-key model access and usage limits."}},
	}
}

type managementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Body    []byte      `json:"Body"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if method == http.MethodGet && path == consolePath {
		return consolePage()
	}
	s := global.get()
	if s == nil {
		return jsonResponse(http.StatusServiceUnavailable, errBody("plugin is not configured"))
	}
	if len(req.Body) > maxBodyBytes {
		return jsonResponse(http.StatusRequestEntityTooLarge, errBody("request body too large"))
	}
	switch {
	case method == http.MethodPost && path == statePath:
		return handleState(s, req.Body)
	case method == http.MethodPut && path == policyPath:
		return handleConfig(s, req.Body, req.Headers.Get("If-Match"))
	case method == http.MethodPost && path == usagePath:
		return handleSetUsage(s, req.Body)
	default:
		return jsonResponse(http.StatusNotFound, errBody("route not found"))
	}
}

func handleState(s *store, body []byte) ([]byte, error) {
	var in struct {
		Scopes []string `json:"scopes"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return jsonResponse(http.StatusBadRequest, errBody("invalid JSON"))
		}
	}
	cfg := s.cfg.Load().cfg
	set := map[string]bool{}
	scopes := make([]string, 0, len(in.Scopes)+len(cfg.Policies))
	add := func(sc string) {
		sc = strings.ToLower(strings.TrimSpace(sc))
		if scopeRE.MatchString(sc) && !set[sc] {
			set[sc] = true
			scopes = append(scopes, sc)
		}
	}
	for _, sc := range in.Scopes {
		add(sc)
	}
	for sc := range cfg.Policies {
		add(sc)
	}
	cfgOut, rev, keys := s.snapshot(scopes)
	return jsonResponse(http.StatusOK, map[string]any{
		"version":  pluginVersion,
		"revision": rev,
		"timezone": s.loc.String(),
		"config":   cfgOut,
		"keys":     keys,
	})
}

func handleConfig(s *store, body []byte, ifMatch string) ([]byte, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return jsonResponse(http.StatusBadRequest, errBody("invalid config: "+err.Error()))
	}
	rev := int64(-1)
	if v := strings.Trim(strings.TrimSpace(ifMatch), `"`); v != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "rev-"), 10, 64)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, errBody("invalid If-Match"))
		}
		rev = n
	}
	newRev, err := s.replaceConfig(cfg, rev)
	if errors.Is(err, errRevision) {
		return jsonResponse(http.StatusPreconditionFailed, errBody("配置已被修改，请刷新后重试"))
	}
	if err != nil {
		return jsonResponse(http.StatusBadRequest, errBody(err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]any{"revision": newRev})
}

func handleSetUsage(s *store, body []byte) ([]byte, error) {
	var in struct {
		Scope   string  `json:"scope"`
		LimitID string  `json:"limit_id"`
		Used    float64 `json:"used"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, errBody("invalid JSON"))
	}
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	if scope != "*" && !scopeRE.MatchString(scope) {
		return jsonResponse(http.StatusBadRequest, errBody("invalid scope"))
	}
	n, err := s.setUsed(scope, strings.TrimSpace(in.LimitID), in.Used)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, errBody(err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]any{"updated": n})
}

func errBody(msg string) map[string]any { return map[string]any{"error": msg} }

func jsonResponse(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           []string{"application/json; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: body,
	})
}
