package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	pluginID   = "key-quota"
	pluginName = "Key Quota"

	// RPC schema 6 keeps management responses byte-exact; schema 2 is the floor
	// for structured interceptor termination.
	rpcSchemaVersion        uint32 = 6
	minimumRPCSchemaVersion uint32 = 2
)

var pluginVersion = "0.1.2"

func main() {}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type pluginConfig struct {
	DataDir  string `yaml:"data_dir"`
	Timezone string `yaml:"timezone"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "request.intercept_before":
		return interceptBefore(request)
	case "request.intercept_after":
		// Accounting happens once, before execution; the after hook is a no-op.
		return okNoop, nil
	case "usage.handle":
		return handleUsage(request)
	case "plugin.register", "plugin.reconfigure":
		return configure(request)
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func configure(raw []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	var cfg pluginConfig
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return nil, fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = "plugins/key-quota"
	}
	if strings.TrimSpace(cfg.Timezone) == "" {
		cfg.Timezone = "Local"
	}
	if err := global.open(cfg); err != nil {
		return nil, err
	}
	schema := req.SchemaVersion
	if schema == 0 || schema > rpcSchemaVersion {
		schema = rpcSchemaVersion
	}
	return okEnvelope(map[string]any{
		"schema_version": schema,
		"metadata": map[string]any{
			"Name":    pluginName,
			"Version": pluginVersion,
			"Author":  "R0smontis",
			// The host rejects registrations with an empty repository field.
			"GitHubRepository": "https://github.com/R0smontis/cpa-key-quota",
			"Logo":             "",
			"ConfigFields": []map[string]string{
				{"Name": "data_dir", "Type": "string", "Description": "Directory for key-quota.json (policies, pricing, counters)."},
				{"Name": "timezone", "Type": "string", "Description": "IANA zone used for hour/day/week/month windows, e.g. Asia/Shanghai."},
			},
		},
		"capabilities": map[string]any{
			"request_interceptor":                  schema >= minimumRPCSchemaVersion,
			"request_interceptor_omit_body_before": true,
			"request_interceptor_omit_body_after":  true,
			"usage_plugin":                         true,
			"management_api":                       true,
		},
	})
}

var okNoop = mustEnvelope(map[string]any{})

func mustEnvelope(v any) []byte {
	raw, err := okEnvelope(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, status int) []byte {
	raw, err := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

// terminate builds an interceptor result that ends the request with an
// OpenAI-style error body, which every CPA client surface understands.
func terminate(status int, errType, message, model string) ([]byte, error) {
	detail := map[string]any{"type": errType, "code": errType, "message": message}
	if model != "" {
		detail["model"] = model
	}
	body, err := json.Marshal(map[string]any{"error": detail})
	if err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{
		"Terminate":       true,
		"StatusCode":      status,
		"ResponseHeaders": http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		"ResponseBody":    body,
	})
}
