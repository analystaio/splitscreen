package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Persistent runtime state.
//
// The config directory is rebuilt from the bundle on every push and lives on
// tmpfs, so by default everything the harness writes into it at runtime — its
// own notes about the project, session transcripts, skills it authors — is gone
// at the next push or reboot. That is right for credentials and for anything the
// bundle owns. It is wrong for what the agent learns: a runner that forgets the
// project every time it is redeployed relearns it at the operators' expense.
//
// With a state directory configured, three things survive:
//
//   - projects/ in the config dir is a symlink into the state dir. It holds the
//     harness's per-project memory and its session transcripts, so notes and
//     --resume both outlive a reboot.
//   - skills/ is a symlink into the state dir. Bundle skills are written there
//     on every push and always win on a name clash; skills created at runtime
//     are left alone. A skill the bundle stops shipping is removed.
//   - CLAUDE.local.md in the state dir is appended to the assembled memory at
//     the start of every session, so humans and the agent have one durable
//     place for notes. Everything else in CLAUDE.md stays bundle-owned.
//
// The thread -> session id map is persisted alongside, since a resume id held
// only in memory would make the surviving transcripts unreachable.

const (
	stateProjects      = "projects"
	stateSkills        = "skills"
	stateLocalMemory   = "CLAUDE.local.md"
	stateSessions      = "sessions.json"
	stateBundleSkills  = ".bundle-skills.json"
	assembledMemory    = "CLAUDE.md"
	bundleMemoryDir    = "memory"
	durableNotesHeader = "## Durable notes"
)

// prepareState links the persistent parts of the state directory into a
// freshly staged config directory. Called by applyBundle before the swap.
func (r *Runner) prepareState(staging string) error {
	state := r.opts.StateDir
	for _, d := range []string{state, filepath.Join(state, stateProjects), filepath.Join(state, stateSkills)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("runner: state dir: %w", err)
		}
	}
	if err := r.adoptEphemeralState(state); err != nil {
		// Losing what the previous tmpfs dir held is the status quo, not a
		// reason to refuse the bundle.
		r.log.Warn("adopting state from the ephemeral config dir failed", "err", err)
	}
	if err := syncBundleSkills(filepath.Join(staging, stateSkills), state); err != nil {
		return err
	}
	for _, name := range []string{stateProjects, stateSkills} {
		dest := filepath.Join(staging, name)
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(state, name), dest); err != nil {
			return fmt.Errorf("runner: link %s into the config dir: %w", name, err)
		}
	}
	return nil
}

// adoptEphemeralState carries a live, pre-state-dir config directory's
// projects/ into an empty state dir, the first time a state dir is used. It is
// what lets a runner that has been running on tmpfs switch to a state dir
// without every thread losing its resume point and memory on the way. Runtime
// skills are not adopted: without the previous bundle's manifest there is no
// telling them apart from bundle skills.
func (r *Runner) adoptEphemeralState(state string) error {
	live := filepath.Join(r.opts.RuntimeRoot, r.opts.Name, "config", stateProjects)
	fi, err := os.Lstat(live)
	if err != nil || !fi.IsDir() {
		return nil // absent, or already a link into the state dir
	}
	dest := filepath.Join(state, stateProjects)
	if entries, err := os.ReadDir(dest); err != nil || len(entries) > 0 {
		return err
	}
	r.log.Info("adopting harness state from the ephemeral config dir", "from", live, "to", dest)
	return copyTree(live, dest)
}

// syncBundleSkills installs the bundle's skills into the persistent skills
// directory. Bundle skills replace whatever is there under the same name;
// skills the previous bundle shipped and this one does not are removed; every
// other directory there was created at runtime and is kept.
func syncBundleSkills(stagedSkills, state string) error {
	dest := filepath.Join(state, stateSkills)
	manifest := filepath.Join(state, stateBundleSkills)

	var previous []string
	if raw, err := os.ReadFile(manifest); err == nil {
		_ = json.Unmarshal(raw, &previous)
	}

	var current []string
	entries, err := os.ReadDir(stagedSkills)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			current = append(current, e.Name())
		}
	}
	sort.Strings(current)

	shipped := map[string]bool{}
	for _, n := range current {
		shipped[n] = true
	}
	for _, n := range previous {
		if !shipped[n] && safeRelPath(n) == nil {
			if err := os.RemoveAll(filepath.Join(dest, n)); err != nil {
				return err
			}
		}
	}
	for _, n := range current {
		target := filepath.Join(dest, n)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := copyTree(filepath.Join(stagedSkills, n), target); err != nil {
			return fmt.Errorf("runner: install skill %q: %w", n, err)
		}
	}

	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return writeFileAtomic(manifest, raw, 0o600)
}

// copyTree copies a directory of regular files. The staging tree is written by
// applyBundle from validated relative paths, so it holds nothing else; anything
// that is not a directory or a regular file is skipped rather than followed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o700)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, in); err != nil {
				f.Close()
				return err
			}
			return f.Close()
		default:
			return nil
		}
	})
}

// localMemoryPath is the durable notes file, or "" without a state dir.
func (r *Runner) localMemoryPath() string {
	if r.opts.StateDir == "" {
		return ""
	}
	return filepath.Join(r.opts.StateDir, stateLocalMemory)
}

// refreshMemory re-assembles CLAUDE.md in the live config dir, so an edit to
// the durable notes reaches the next session without waiting for a push.
func (r *Runner) refreshMemory(configDir string) error {
	if r.opts.StateDir == "" {
		return nil
	}
	return assembleMemory(configDir, r.localMemoryPath())
}

// ---------------------------------------------------------------------------
// Session ids
// ---------------------------------------------------------------------------

// sessionIndex persists thread -> harness session id. Without it a restarted
// runner starts every thread fresh even though the transcripts it would resume
// are still on disk.
type sessionIndex struct {
	mu   sync.Mutex
	path string
	ids  map[string]string
}

func loadSessionIndex(stateDir string) (*sessionIndex, error) {
	idx := &sessionIndex{ids: map[string]string{}}
	if stateDir == "" {
		return idx, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("runner: state dir: %w", err)
	}
	idx.path = filepath.Join(stateDir, stateSessions)
	raw, err := os.ReadFile(idx.path)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &idx.ids); err != nil {
		// A corrupt index costs resumes, not correctness: start empty rather
		// than refusing to run.
		idx.ids = map[string]string{}
	}
	return idx, nil
}

func (s *sessionIndex) get(thread string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[thread]
}

// set records a session id; an empty id forgets the thread.
func (s *sessionIndex) set(thread, id string) error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[thread] == id {
		return nil
	}
	if id == "" {
		delete(s.ids, thread)
	} else {
		s.ids[thread] = id
	}
	raw, err := json.Marshal(s.ids)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, raw, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
