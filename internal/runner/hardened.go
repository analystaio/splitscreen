package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/analystaio/splitscreen/internal/harness"
)

// Hardened mode (docs/secrets-broker.md §8.4). Attribution is only as good as
// the runner doing it. If the agent's Unix user could change the runner's
// unit, swap its binary, swap the harness binary, or trace its ancestors, it
// could make the runner say anything. The runner checks each of those about
// itself at start, and reports the result in hello. The gateway believes an
// attributed request only from a runner that passed and that the operator also
// marked hardened.

// hardenedCheck returns the problems found; none means hardened.
func (r *Runner) hardenedCheck() []string {
	if runtime.GOOS != "linux" {
		return []string{"hardened mode is only checked on Linux"}
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if b, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope"); err != nil {
		add("Yama is not enabled, so the agent could trace the runner and the harness")
	} else if strings.TrimSpace(string(b)) == "0" {
		add("kernel.yama.ptrace_scope is 0, so the agent could trace the runner and the harness (set it to 1)")
	}

	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		cg := string(b)
		if !strings.Contains(cg, "/system.slice/") {
			add("the runner is not a system service (cgroup %s), so its own user can change how it is started", strings.TrimSpace(cg))
		}
	} else {
		add("cannot read the runner's cgroup: %v", err)
	}

	for _, k := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT"} {
		if os.Getenv(k) != "" {
			add("%s is set in the runner's environment", k)
		}
	}

	uid := os.Getuid()
	if self, err := os.Executable(); err != nil {
		add("cannot find the runner's own binary: %v", err)
	} else if p := writableBy(self, uid); p != "" {
		add("the runner binary %s can be changed by uid %d (%s)", self, uid, p)
	}

	bin := "claude"
	if ba, ok := r.adapter.(harness.BinaryAdapter); ok {
		bin = ba.BinaryPath()
	}
	if path, err := exec.LookPath(bin); err != nil {
		add("cannot find the harness binary %q: %v", bin, err)
	} else if p := writableBy(path, uid); p != "" {
		add("the harness binary %s can be changed by uid %d (%s)", path, uid, p)
	}
	return problems
}

// writableBy reports how uid could change the file at path: by owning or being
// able to write it, any directory on the way to it, or any symlink in that
// chain's targets. Empty means it cannot.
func writableBy(path string, uid int) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "cannot resolve: " + err.Error()
	}
	// Check the path as given and as resolved: a symlink in a writable
	// directory can be repointed even if its target is safe.
	for _, p := range uniq(path, real) {
		for cur := filepath.Clean(p); ; cur = filepath.Dir(cur) {
			if why := mutableBy(cur, uid); why != "" {
				return why
			}
			if cur == "/" || cur == "." {
				break
			}
		}
	}
	return ""
}

func mutableBy(p string, uid int) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return p + ": " + err.Error()
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return p + ": no ownership information"
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// A symlink's own mode is meaningless; its directory decides.
		return ""
	}
	if int(st.Uid) == uid && uid != 0 {
		return p + " is owned by it"
	}
	if fi.Mode().Perm()&0o002 != 0 && !(fi.IsDir() && fi.Mode()&os.ModeSticky != 0) {
		return p + " is world-writable"
	}
	if fi.Mode().Perm()&0o020 != 0 && inGroup(int(st.Gid)) {
		return p + " is writable by a group it is in"
	}
	return ""
}

func inGroup(gid int) bool {
	if os.Getgid() == gid {
		return true
	}
	gs, _ := os.Getgroups()
	for _, g := range gs {
		if g == gid {
			return true
		}
	}
	return false
}

func uniq(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}
