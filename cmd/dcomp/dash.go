package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/lifecycle"
)

//go:embed all:dashboard
var dashboardFS embed.FS

// dashboardContent is the served viewer tree: index.html, style.css, and the
// ES modules under js/. No build step and no external asset exists.
var dashboardContent = func() fs.FS {
	sub, err := fs.Sub(dashboardFS, "dashboard")
	if err != nil {
		panic(err)
	}
	return sub
}()

// dashBackend is the observational surface the dash server needs. The
// concrete implementation is the lifecycle controller plus its state store;
// tests supply an in-memory one.
type dashBackend interface {
	Systems() ([]string, error)
	Status(ctx context.Context, name string) (lifecycle.Status, error)
}

type controllerBackend struct {
	controller *lifecycle.Controller
}

func (backend controllerBackend) Systems() ([]string, error) {
	return backend.controller.State.Systems()
}

func (backend controllerBackend) Status(
	ctx context.Context,
	name string,
) (lifecycle.Status, error) {
	return backend.controller.Status(ctx, name)
}

type systemsDocument struct {
	APIVersion int      `json:"api_version"`
	Systems    []string `json:"systems"`
}

// dashServer serves the embedded viewer and the read-only view API. It never
// mutates state or Docker resources.
type dashServer struct {
	backend dashBackend
	// selected restricts the served state systems when non-empty.
	selected map[string]struct{}
	// files maps a system name to a system file served as configuration.
	files map[string]string
	// cacheFor bounds how stale a served view may be; it protects the Docker
	// engine from concurrent viewers, not correctness.
	cacheFor time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedView
}

type cachedView struct {
	observed time.Time
	document viewDocument
}

func newDashServer(
	backend dashBackend,
	systems []string,
	files map[string]string,
) *dashServer {
	server := &dashServer{
		backend:  backend,
		files:    files,
		cacheFor: time.Second,
		now:      time.Now,
		cache:    make(map[string]cachedView),
	}
	if len(systems) != 0 || len(files) != 0 {
		server.selected = make(map[string]struct{}, len(systems))
		for _, name := range systems {
			server.selected[name] = struct{}{}
		}
	}
	return server
}

func (server *dashServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", server.handlePage(http.FileServerFS(dashboardContent)))
	mux.HandleFunc("/api/v2/systems", server.handleSystems)
	mux.HandleFunc("/api/v2/view/", server.handleView)
	return mux
}

func (server *dashServer) handlePage(files http.Handler) http.Handler {
	return http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(response, request)
	})
}

func (server *dashServer) handleSystems(
	response http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	names, err := server.systems()
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	_ = writeDocument(response, systemsDocument{
		APIVersion: apiVersion,
		Systems:    names,
	})
}

func (server *dashServer) handleView(
	response http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(request.URL.Path, "/api/v2/view/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(response, request)
		return
	}
	if !server.serves(name) {
		http.NotFound(response, request)
		return
	}
	document, err := server.view(request.Context(), name)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	_ = writeDocument(response, document)
}

func (server *dashServer) serves(name string) bool {
	if _, file := server.files[name]; file {
		return true
	}
	if server.selected == nil {
		return true
	}
	_, exists := server.selected[name]
	return exists
}

func (server *dashServer) systems() ([]string, error) {
	names, err := server.backend.Systems()
	if err != nil {
		return nil, err
	}
	selected := make([]string, 0, len(names)+len(server.files))
	seen := make(map[string]struct{}, len(names)+len(server.files))
	for _, name := range names {
		if server.serves(name) {
			selected = append(selected, name)
			seen[name] = struct{}{}
		}
	}
	for name := range server.files {
		if _, exists := seen[name]; exists {
			continue
		}
		selected = append(selected, name)
	}
	sort.Strings(selected)
	return selected, nil
}

func (server *dashServer) view(
	ctx context.Context,
	name string,
) (viewDocument, error) {
	server.mu.Lock()
	cached, exists := server.cache[name]
	server.mu.Unlock()
	if exists && server.now().Sub(cached.observed) < server.cacheFor {
		return cached.document, nil
	}
	var document viewDocument
	if path, file := server.files[name]; file {
		spec, err := composition.Load(path)
		if err != nil {
			return viewDocument{}, err
		}
		document = viewFromSpec(spec)
	} else {
		status, err := server.backend.Status(ctx, name)
		if err != nil {
			return viewDocument{}, err
		}
		document = viewFromStatus(status)
	}
	server.mu.Lock()
	server.cache[name] = cachedView{
		observed: server.now(),
		document: document,
	}
	server.mu.Unlock()
	return document, nil
}

// runDash serves until ctx is cancelled. The listener is already bound so the
// caller can print the effective address before blocking.
func runDash(ctx context.Context, listener net.Listener, server *dashServer) error {
	httpServer := &http.Server{
		Handler:           server.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		errs <- httpServer.Serve(listener)
	}()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve dash: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	<-errs
	return nil
}
