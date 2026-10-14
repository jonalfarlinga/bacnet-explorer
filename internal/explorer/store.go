package explorer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

	// infos holds what has been counted in each registry file. A registry
	// file never changes, so it is counted once.
	mu    sync.Mutex
	infos map[string]RegistryInfo
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

// RegistryInfo describes one registry file for the admin.
type RegistryInfo struct {
	Name    string    `json:"name"`
	Written time.Time `json:"written"`
	// InForce marks the latest file, which is the registry.
	InForce   bool `json:"inForce"`
	Devices   int  `json:"devices"`
	Tracked   int  `json:"tracked"`
	Sidelined int  `json:"sidelined"`
}

// RegistryFile is a registry file with what it holds.
type RegistryFile struct {
	RegistryInfo
	Nodes []Node `json:"nodes"`
}

// OpenStore opens the directory, creating it if need be.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{dir: dir, infos: map[string]RegistryInfo{}}, nil
}

// Load reads the latest registry and the temp file. A missing file is an
// empty one.
func (s *Store) Load() (registry, temp []Node, err error) {
	latest, _, err := s.latest()
	if err != nil {
		return nil, nil, err
	}
	if latest != "" {
		f, err := s.read(latest)
		if err != nil {
			return nil, nil, err
		}
		registry = f.Nodes
	}
	f, err := s.read(tempFile)
	if err != nil {
		return nil, nil, err
	}
	return registry, f.Nodes, nil
}

// Info describes the store.
func (s *Store) Info() StoreInfo {
	latest, count, _ := s.latest()
	return StoreInfo{Dir: s.dir, Latest: latest, Registries: count}
}

// latest finds the registry file in force: the last by name, which is by
// datetime.
func (s *Store) latest() (name string, count int, err error) {
	names, err := s.registryNames()
	if err != nil || len(names) == 0 {
		return "", 0, err
	}
	return names[len(names)-1], len(names), nil
}

// registryNames lists the registry files, oldest first.
func (s *Store) registryNames() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && isRegistryName(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

func isRegistryName(name string) bool {
	return strings.HasPrefix(name, registryPrefix) && strings.HasSuffix(name, registrySuffix)
}

// Registries describes every registry file, the latest first.
func (s *Store) Registries() ([]RegistryInfo, error) {
	names, err := s.registryNames()
	if err != nil {
		return nil, err
	}
	list := make([]RegistryInfo, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- {
		info, err := s.registryInfo(names[i])
		if err != nil {
			return nil, err
		}
		info.InForce = i == len(names)-1
		list = append(list, info)
	}
	return list, nil
}

// ReadRegistry reads one registry file, the latest or one of the history.
func (s *Store) ReadRegistry(name string) (RegistryFile, error) {
	// The name comes from outside: it names a registry file in the
	// directory, and nothing else.
	if !isRegistryName(name) || name != filepath.Base(name) {
		return RegistryFile{}, fmt.Errorf("%w: %q", ErrUnknownRegistry, name)
	}
	if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
		return RegistryFile{}, fmt.Errorf("%w: %q", ErrUnknownRegistry, name)
	}
	f, err := s.read(name)
	if err != nil {
		return RegistryFile{}, err
	}
	latest, _, err := s.latest()
	if err != nil {
		return RegistryFile{}, err
	}
	info := s.count(name, f)
	info.InForce = name == latest
	return RegistryFile{RegistryInfo: info, Nodes: f.Nodes}, nil
}

// registryInfo counts what a registry file holds, reading it the first time.
func (s *Store) registryInfo(name string) (RegistryInfo, error) {
	s.mu.Lock()
	info, ok := s.infos[name]
	s.mu.Unlock()
	if ok {
		return info, nil
	}
	f, err := s.read(name)
	if err != nil {
		return RegistryInfo{}, err
	}
	return s.count(name, f), nil
}

func (s *Store) count(name string, f storeFile) RegistryInfo {
	info := RegistryInfo{Name: name, Written: f.Written}
	for _, n := range f.Nodes {
		switch {
		case n.Kind == KindDevice:
			info.Devices++
		case n.Track == TrackTracked:
			info.Tracked++
		case n.Track == TrackSidelined:
			info.Sidelined++
		}
	}
	s.mu.Lock()
	s.infos[name] = info
	s.mu.Unlock()
	return info
}

func (s *Store) read(name string) (storeFile, error) {
	var f storeFile
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("store: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("store: %s: %w", name, err)
	}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		n.Reached = false
		// Nothing under a loaded node has been read in this run.
		if n.Children.State != ChildrenNone && n.Children.State != "" {
			n.Children = ChildrenState{State: ChildrenUnknown, Total: n.Children.Total}
		}
	}
	return f, nil
}

// WriteTemp replaces the temp file.
func (s *Store) WriteTemp(nodes []Node) error {
	return s.write(tempFile, nodes, 0o644)
}

// WriteRegistry writes a new registry file, named by the moment, and
// returns its name. A registry file is never written again, so it is
// read-only from the moment it appears.
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
	return name, s.write(name, nodes, 0o444)
}

// write puts the nodes in a file, whole: it is written beside the file and
// moved into place, so a reader never sees half of it. The file has its
// permissions before it is moved, so it is never in place with others.
func (s *Store) write(name string, nodes []Node, perm os.FileMode) error {
	if nodes == nil {
		nodes = []Node{}
	}
	b, err := json.MarshalIndent(storeFile{Written: time.Now(), Nodes: nodes}, "", "  ")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: %w", err)
	}
	return nil
}
