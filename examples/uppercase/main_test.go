package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminHandler(t *testing.T) {
	server := &uppercaseServer{}
	server.requests.Store(7)
	handler := server.adminHandler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health response = %d %q", health.Code, health.Body.String())
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK ||
		!strings.Contains(
			metrics.Body.String(),
			"dcomp_example_uppercase_requests_total 7\n",
		) {
		t.Fatalf("metrics response = %d %q", metrics.Code, metrics.Body.String())
	}

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status = %d, want 405", post.Code)
	}
}
