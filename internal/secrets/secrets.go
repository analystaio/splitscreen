// Package secrets resolves named secrets for the gateway.
//
// Bundles and MCP declarations reference secrets by name; values are resolved
// here at materialization time. Nothing in a versioned, diffable config ever
// contains a secret value — otherwise the config becomes a secret store and the
// credential model unravels at the last step.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Secret is a resolved value plus the metadata the gateway needs to warn before
// it expires.
type Secret struct {
	Name      string
	Value     string
	ExpiresAt *time.Time
}

// Backend resolves secrets by name. Implementations must be safe for concurrent
// use.
type Backend interface {
	Get(name string) (Secret, error)
	// Names lists what the backend knows about, for expiry sweeps and status
	// output. Backends that cannot enumerate return an empty slice.
	Names() []string
}

// ErrNotFound is returned for an unknown secret name.
var ErrNotFound = fmt.Errorf("secrets: not found")

// ---------------------------------------------------------------------------

// EnvBackend reads SPLITSCREEN_SECRET_<UPPER_NAME> from the environment. Useful
// for containers and for getting started; it cannot express expiry.
type EnvBackend struct{ Prefix string }

func NewEnvBackend() *EnvBackend { return &EnvBackend{Prefix: "SPLITSCREEN_SECRET_"} }

func (e *EnvBackend) key(name string) string {
	return e.Prefix + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

func (e *EnvBackend) Get(name string) (Secret, error) {
	v, ok := os.LookupEnv(e.key(name))
	if !ok {
		return Secret{}, fmt.Errorf("%w: %s (looked for %s)", ErrNotFound, name, e.key(name))
	}
	return Secret{Name: name, Value: v}, nil
}

func (e *EnvBackend) Names() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, e.Prefix) {
			out = append(out, strings.ToLower(strings.TrimPrefix(k, e.Prefix)))
		}
	}
	return out
}

// ---------------------------------------------------------------------------

// DirBackend reads one file per secret from a directory. A sibling
// "<name>.expires" file, if present, carries an RFC3339 expiry so the gateway
// can warn ahead of an outage rather than discovering it.
type DirBackend struct {
	dir string
	mu  sync.RWMutex
}

func NewDirBackend(dir string) (*DirBackend, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("secrets: %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("secrets: %s is mode %o; it must not be readable by group or other", dir, perm)
	}
	return &DirBackend{dir: dir}, nil
}

func safeName(name string) error {
	if name == "" {
		return fmt.Errorf("secrets: empty name")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("secrets: %q must be a bare name", name)
	}
	return nil
}

func (d *DirBackend) Get(name string) (Secret, error) {
	if err := safeName(name); err != nil {
		return Secret{}, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	raw, err := os.ReadFile(filepath.Join(d.dir, name))
	if os.IsNotExist(err) {
		return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("secrets: read %s: %w", name, err)
	}
	s := Secret{Name: name, Value: strings.TrimRight(string(raw), "\r\n")}

	if exp, err := os.ReadFile(filepath.Join(d.dir, name+".expires")); err == nil {
		t, perr := time.Parse(time.RFC3339, strings.TrimSpace(string(exp)))
		if perr != nil {
			return Secret{}, fmt.Errorf("secrets: %s.expires is not RFC3339: %w", name, perr)
		}
		s.ExpiresAt = &t
	}
	return s, nil
}

func (d *DirBackend) Names() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".expires") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// ---------------------------------------------------------------------------

// Chain tries each backend in order, so an operator can layer a directory over
// the environment without either knowing about the other.
type Chain []Backend

func (c Chain) Get(name string) (Secret, error) {
	for _, b := range c {
		s, err := b.Get(name)
		if err == nil {
			return s, nil
		}
		if !isNotFound(err) {
			return Secret{}, err
		}
	}
	return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

func (c Chain) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range c {
		for _, n := range b.Names() {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// Invalidator is implemented by backends that cache. Dropping a name forces
// the next Get to the source.
type Invalidator interface {
	Invalidate(name string)
}

// Invalidate forwards to every backend in the chain that caches.
func (c Chain) Invalidate(name string) {
	for _, b := range c {
		if inv, ok := b.(Invalidator); ok {
			inv.Invalidate(name)
		}
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "secrets: not found")
}

// Expiring returns secrets due to expire within d. The gateway warns on these
// so a mandatory token rotation becomes a calendar item rather than an outage.
func Expiring(b Backend, within time.Duration, now time.Time) []Secret {
	var out []Secret
	for _, name := range b.Names() {
		s, err := b.Get(name)
		if err != nil || s.ExpiresAt == nil {
			continue
		}
		if s.ExpiresAt.Sub(now) <= within {
			s.Value = "" // never carry the value into a warning path
			out = append(out, s)
		}
	}
	return out
}
