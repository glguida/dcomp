package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/lifecycle"
)

type fakeBackend struct {
	systems     []string
	statusCalls int
}

func (backend *fakeBackend) Systems() ([]string, error) {
	return backend.systems, nil
}

func (backend *fakeBackend) Status(
	_ context.Context,
	name string,
) (lifecycle.Status, error) {
	backend.statusCalls++
	return lifecycle.Status{Name: name}, nil
}

func TestDashServesEmbeddedPage(t *testing.T) {
	server := newDashServer(&fakeBackend{}, nil, nil)
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("page status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("page content type = %q", contentType)
	}
	if !strings.Contains(recorder.Body.String(), "dcomp · system view") {
		t.Fatalf("page body does not look like the viewer")
	}
	if strings.Contains(recorder.Body.String(), "API 1") ||
		!strings.Contains(recorder.Body.String(), `id="api-version"`) {
		t.Fatalf("page hardcodes a stale API version")
	}
}

func TestDashSystemsAndViewDocuments(t *testing.T) {
	backend := &fakeBackend{systems: []string{"alpha", "beta"}}
	server := newDashServer(backend, nil, nil)
	handler := server.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/systems", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("systems status = %d", recorder.Code)
	}
	var systems systemsDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &systems); err != nil {
		t.Fatal(err)
	}
	if systems.APIVersion != apiVersion || len(systems.Systems) != 2 {
		t.Fatalf("systems document = %+v", systems)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/view/alpha", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("view status = %d", recorder.Code)
	}
	var document viewDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Name != "alpha" || document.Source != "state" {
		t.Fatalf("view document = %+v", document)
	}
}

func TestDashSelectionRestrictsServedSystems(t *testing.T) {
	backend := &fakeBackend{systems: []string{"alpha", "beta"}}
	server := newDashServer(backend, []string{"beta"}, nil)
	handler := server.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/systems", nil),
	)
	var systems systemsDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &systems); err != nil {
		t.Fatal(err)
	}
	if len(systems.Systems) != 1 || systems.Systems[0] != "beta" {
		t.Fatalf("restricted systems = %+v", systems.Systems)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/view/alpha", nil),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unselected system status = %d, want 404", recorder.Code)
	}
}

func TestDashSystemsDeduplicatesFileOverRecordedSystem(t *testing.T) {
	backend := &fakeBackend{systems: []string{"alpha"}}
	server := newDashServer(
		backend,
		[]string{"alpha"},
		map[string]string{"alpha": "/unused/system.dcomp"},
	)

	names, err := server.systems()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("systems = %+v, want one alpha", names)
	}
}

func TestDashViewIsCachedWithinTheObservationWindow(t *testing.T) {
	backend := &fakeBackend{systems: []string{"alpha"}}
	server := newDashServer(backend, nil, nil)
	now := time.Unix(1000, 0)
	server.now = func() time.Time { return now }
	handler := server.handler()

	for i := 0; i < 3; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodGet, "/api/v2/view/alpha", nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("view status = %d", recorder.Code)
		}
	}
	if backend.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1 within cache window", backend.statusCalls)
	}

	now = now.Add(2 * time.Second)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/view/alpha", nil),
	)
	if backend.statusCalls != 2 {
		t.Fatalf("status calls = %d, want 2 after expiry", backend.statusCalls)
	}
}

func TestDashRejectsNonGetAndTraversal(t *testing.T) {
	server := newDashServer(&fakeBackend{systems: []string{"alpha"}}, nil, nil)
	handler := server.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/api/v2/view/alpha", nil),
	)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/view/alpha/extra", nil),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("nested path status = %d, want 404", recorder.Code)
	}
}
