package explorer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The store is two kinds of file in one directory. temp.json holds what has
// been discovered and not decided about: it is disposable, rewritten as
// discovery goes. registry_<datetime>.json holds the points the admin has
// decided about, with their devices; each decision writes a new one, and
// the latest is the whole registry, so the older ones are its history.
const (
	tempFile       = "temp.json"
	registryPrefix = "registry_"
	registrySuffix = ".json"
)

// Store is the directory the explorer keeps what it knows in.
type Store struct {
	dir string
}

// A file of the store.
type storeFile struct {
	Written time.Time `json:"written"`
	Nodes   []Node    `json:"nodes"`
}

// StoreInfo describes the store for the admin.
type StoreInfo struct {
	Dir string `json:"dir"`
	// Latest is the name of the registry file in force, "" before the
	// first decision.
	Latest string `json:"latest,omitempty"`
	// Registries counts the registry files, the latest included.
	Registries int `json:"registries"`
}

// OpenStore opens the directory, creating it if need be.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Load reads the latest registry and the temp file. A missing file is an
// empty one.
func (s *Store) Load() (registry, temp []Node, err error) {
	latest, _, err := s.latest()
	if err != nil {
		return nil, nil, err
	}
	if latest != "" {
		if registry, err = s.read(latest); err != nil {
			return nil, nil, err
		}
	}
	if temp, err = s.read(tempFile); err != nil {
		return nil, nil, err
	}
	return registry, temp, nil
}

// Info describes the store.
func (s *Store) Info() StoreInfo {
	latest, count, _ := s.latest()
	return StoreInfo{Dir: s.dir, Latest: latest, Registries: count}
}

// latest finds the registry file in force: the last by name, which is by
// datetime.
func (s *Store) latest() (name string, count int, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, registryPrefix) && strings.HasSuffix(n, registrySuffix) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", 0, nil
	}
	sort.Strings(names)
	return names[len(names)-1], len(names), nil
}

func (s *Store) read(name string) ([]Node, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("store: %s: %w", name, err)
	}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		n.Reached = false
		// Nothing under a loaded node has been read in this run.
		if n.Children.State != ChildrenNone && n.Children.State != "" {
			n.Children = ChildrenState{State: ChildrenUnknown, Total: n.Children.Total}
		}
	}
	return f.Nodes, nil
}

// WriteTemp replaces the temp file.
func (s *Store) WriteTemp(nodes []Node) error {
	return s.write(tempFile, nodes)
}

// WriteRegistry writes a new registry file, named by the moment, and
// returns its name.
func (s *Store) WriteRegistry(nodes []Node) (string, error) {
	// Milliseconds, and a counter after them if need be, so that a later
	// file always sorts later: "_" sorts after ".".
	stamp := time.Now().Format("20060102-150405.000")
	name := registryPrefix + stamp + registrySuffix
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(s.dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		name = fmt.Sprintf("%s%s_%d%s", registryPrefix, stamp, i, registrySuffix)
	}
	return name, s.write(name, nodes)
}

// write puts the nodes in a file, whole: it is written beside the file and
// moved into place, so a reader never sees half of it.
func (s *Store) write(name string, nodes []Node) error {
	if nodes == nil {
		nodes = []Node{}
	}
	b, err := json.MarshalIndent(storeFile{Written: time.Now(), Nodes: nodes}, "", "  ")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: %w", err)
	}
	return nil
}
