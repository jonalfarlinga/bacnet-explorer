// Package api serves the explorer's HTTP API and its web UI.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonalfarlinga/bacnet/internal/client"
	"github.com/jonalfarlinga/bacnet/internal/discovery"
)

// Config is what the API serves.
type Config struct {
	Service *discovery.Service
	// Select opens the BACnet socket on the named adapter. It is nil when the
	// adapter cannot be changed, as with simulated devices.
	Select func(adapter string) error
	// UI holds the built web UI, with index.html at its root.
	UI fs.FS
}

type adapter struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Broadcast string `json:"broadcast"`
}

type status struct {
	Transport *discovery.Transport `json:"transport"`
	// Adapters lists what the BACnet socket can be opened on. It is empty
	// when the adapter cannot be changed.
	Adapters []adapter `json:"adapters"`
}

// New returns the handler for everything the explorer serves.
func New(cfg Config) http.Handler {
	s := cfg.Service
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := status{Transport: s.Transport(), Adapters: []adapter{}}
		if cfg.Select != nil {
			list, err := client.Interfaces()
			if err != nil {
				fail(w, err)
				return
			}
			for _, i := range list {
				st.Adapters = append(st.Adapters, adapter{
					Name: i.Name, Address: i.Addr.String(), Broadcast: i.Broadcast.String(),
				})
			}
		}
		reply(w, st)
	})

	mux.HandleFunc("POST /api/transport", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Adapter string `json:"adapter"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if cfg.Select == nil {
			http.Error(w, "the adapter cannot be changed", http.StatusConflict)
			return
		}
		if err := cfg.Select(body.Adapter); err != nil {
			fail(w, err)
			return
		}
		reply(w, s.Transport())
	})

	mux.HandleFunc("POST /api/discovery", func(w http.ResponseWriter, r *http.Request) {
		if !readJSON(w, r, nil) {
			return
		}
		if err := s.Scan(); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		reply(w, s.Devices())
	})

	objectsOf := func(refresh bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, ok := number(w, r, "id", client.WildcardInstance-1)
			if !ok {
				return
			}
			if refresh && !readJSON(w, r, nil) {
				return
			}
			rev, objs, err := s.Objects(uint32(id), refresh)
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, map[string]any{"rev": rev, "objects": objs})
		}
	}
	mux.HandleFunc("GET /api/devices/{id}/objects", objectsOf(false))
	mux.HandleFunc("POST /api/devices/{id}/refresh", objectsOf(true))

	mux.HandleFunc("GET /api/devices/{id}/objects/{type}/{instance}/properties", func(w http.ResponseWriter, r *http.Request) {
		id, ok1 := number(w, r, "id", client.WildcardInstance-1)
		typ, ok2 := number(w, r, "type", 1023)
		inst, ok3 := number(w, r, "instance", client.WildcardInstance)
		if !ok1 || !ok2 || !ok3 {
			return
		}
		props, err := s.Properties(r.Context(), uint32(id), client.ObjectID{Type: uint16(typ), Instance: uint32(inst)})
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]any{"properties": props, "readAt": time.Now()})
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		events(w, r, s)
	})

	mux.HandleFunc("/api/", http.NotFound)
	mux.Handle("/", ui(cfg.UI))
	return mux
}

// events streams the service's events as server-sent events. The stream ends
// when the client falls too far behind; it reconnects and reloads.
func events(w http.ResponseWriter, r *http.Request, s *discovery.Service) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.Subscribe()
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
	case errors.Is(err, discovery.ErrUnknownDevice):
		code = http.StatusNotFound
	case errors.Is(err, discovery.ErrNoTransport):
		code = http.StatusConflict
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

func number(w http.ResponseWriter, r *http.Request, name string, limit uint64) (uint64, bool) {
	n, err := strconv.ParseUint(r.PathValue(name), 10, 32)
	if err != nil || n > limit {
		http.Error(w, "bad "+name, http.StatusBadRequest)
		return 0, false
	}
	return n, true
}
