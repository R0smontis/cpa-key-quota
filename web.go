package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"
)

//go:embed web/console.html
var consoleTemplate string

func consolePage() ([]byte, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "'",
		"style-src 'nonce-" + nonce + "'",
		"connect-src 'self'",
		"img-src data:",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'self'",
	}, "; ")
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"Content-Security-Policy": []string{csp},
			"Referrer-Policy":         []string{"no-referrer"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"X-Frame-Options":         []string{"SAMEORIGIN"},
		},
		Body: []byte(strings.ReplaceAll(consoleTemplate, "{{NONCE}}", nonce)),
	})
}
