// Package api serves the explorer's HTTP API and its web UI. It knows nodes
// and sources, not protocols.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/explorer"
)

// Config is what the API serves.
type Config struct {
	Registry *explorer.Registry
	// UI holds the built web UI, with index.html at its root.
	UI fs.FS
}

// New returns the handler for everything the explorer serves.
func New(cfg Config) http.Handler {
	reg := cfg.Registry
	mux := http.NewServeMux()

	// ---- sources ----

	mux.HandleFunc("GET /api/sources", func(w http.ResponseWriter, r *http.Request) {
		reply(w, reg.Sources())
	})

	mux.HandleFunc("POST /api/sources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind     string          `json:"kind"`
			Settings explorer.Values `json:"settings"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if err := reg.Open(r.Context(), body.Kind, body.Settings); err != nil {
			fail(w, err)
			return
		}
		reply(w, reg.Sources())
	})

	mux.HandleFunc("DELETE /api/sources/{kind}", func(w http.ResponseWriter, r *http.Request) {
		if err := reg.Close(r.PathValue("kind")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/sources/{kind}/discover", func(w http.ResponseWriter, r *http.Request) {
		if !readJSON(w, r, nil) {
			return
		}
		if err := reg.Discover(r.Context(), r.PathValue("kind")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Adds a node by hand, from the source's add form.
	mux.HandleFunc("POST /api/sources/{kind}/nodes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Values explorer.Values `json:"values"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		n, err := reg.Add(ctx, r.PathValue("kind"), body.Values)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, n)
	})

	// ---- nodes ----
	// Node IDs hold slashes, so they travel as the id query parameter.

	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		reply(w, reg.Nodes())
	})

	mux.HandleFunc("GET /api/node", func(w http.ResponseWriter, r *http.Request) {
		id, ok := nodeID(w, r)
		if !ok {
			return
		}
		n, found := reg.Node(id)
		if !found {
			fail(w, explorer.ErrUnknownNode)
			return
		}
		reply(w, n)
	})

	childrenOf := func(refresh bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, ok := nodeID(w, r)
			if !ok {
				return
			}
			if refresh && !readJSON(w, r, nil) {
				return
			}
			rev, nodes, err := reg.Children(id, refresh)
			if err != nil {
				fail(w, err)
				return
			}
			if nodes == nil {
				nodes = []explorer.Node{}
			}
			reply(w, map[string]any{"rev": rev, "nodes": nodes})
		}
	}
	mux.HandleFunc("GET /api/node/children", childrenOf(false))
	mux.HandleFunc("POST /api/node/refresh", childrenOf(true))

	mux.HandleFunc("GET /api/node/detail", func(w http.ResponseWriter, r *http.Request) {
		id, ok := nodeID(w, r)
		if !ok {
			return
		}
		d, err := reg.Detail(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		if d.Rows == nil {
			d.Rows = []explorer.DetailRow{}
		}
		reply(w, d)
	})

	// ---- the registry of decisions ----

	mux.HandleFunc("GET /api/store", func(w http.ResponseWriter, r *http.Request) {
		info, ok := reg.StoreInfo()
		if !ok {
			http.Error(w, "the explorer runs without a store", http.StatusNotFound)
			return
		}
		reply(w, info)
	})

	// Records the admin's decision about some points.
	mux.HandleFunc("POST /api/classify", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs   []explorer.NodeID `json:"ids"`
			Track explorer.Track    `json:"track"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if len(body.IDs) == 0 || len(body.IDs) > 10000 {
			http.Error(w, "give 1 to 10000 node ids", http.StatusBadRequest)
			return
		}
		nodes, err := reg.Classify(body.IDs, body.Track)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, nodes)
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		events(w, r, reg)
	})

	mux.HandleFunc("/api/", http.NotFound)
	mux.Handle("/", ui(cfg.UI))
	return mux
}

func nodeID(w http.ResponseWriter, r *http.Request) (explorer.NodeID, bool) {
	id := r.URL.Query().Get("id")
	if id == "" || !strings.Contains(id, ":") || len(id) > 512 {
		http.Error(w, "bad node id", http.StatusBadRequest)
		return "", false
	}
	return explorer.NodeID(id), true
}

// events streams the registry's events as server-sent events. The stream
// ends when the client falls too far behind; it reconnects and reloads.
func events(w http.ResponseWriter, r *http.Request, reg *explorer.Registry) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	ch, cancel := reg.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, "retry: 1000\n\n")
	flusher.Flush()

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case e, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(e.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
		}
		flusher.Flush()
	}
}

// ui serves the built web UI, and its index.html for any path that is not a
// file in it.
func ui(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(dist, "index.html"); err != nil {
			http.Error(w, `The web UI has not been built. Run "npm install" and "npm run build" in web/, then build the explorer again.`, http.StatusServiceUnavailable)
			return
		}
		if name := strings.TrimPrefix(r.URL.Path, "/"); name != "" {
			if _, err := fs.Stat(dist, name); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, explorer.ErrUnknownNode), errors.Is(err, explorer.ErrUnknownKind):
		code = http.StatusNotFound
	case errors.Is(err, explorer.ErrNotOpen):
		code = http.StatusConflict
	case errors.Is(err, explorer.ErrNotSupported):
		code = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// readJSON decodes the body of a request that changes something into v. It
// insists on a JSON content type, which a page on another site cannot send
// without leave from this server, so no other site can make the explorer act.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "the body must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if v == nil {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		http.Error(w, "the body is not the JSON expected", http.StatusBadRequest)
		return false
	}
	return true
}
