package runner

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/analystaio/splitscreen/internal/harness"
)

// Attribution answers "which thread's agent opened this connection?" from the
// kernel, not from anything the agent says. Every session on a runner runs as
// one Unix user, so a thread id in a header, an argument or an environment
// variable can be forged by any of them (docs/secrets-broker.md §8).
//
// The runner knows the PID and start time of each harness it started. A
// connection belongs to a thread when the process holding its client socket is
// that harness or one of its descendants. A process that escaped the tree (a
// double-forked daemon) belongs to nobody.

// harnessProc is one live harness process.
type harnessProc struct {
	thread string
	pid    int
	start  uint64
}

// liveHarnesses lists the harness processes of every running session.
func (r *Runner) liveHarnesses() []harnessProc {
	var out []harnessProc
	r.sessions.Range(func(_, v any) bool {
		ts := v.(*threadSession)
		ts.sessionMu.Lock()
		sess := ts.sess
		ts.sessionMu.Unlock()
		if sess == nil || !sess.Running() {
			return true
		}
		if ps, ok := sess.(harness.ProcessSession); ok {
			if pid, start := ps.Process(); pid > 0 {
				out = append(out, harnessProc{thread: ts.threadID, pid: pid, start: start})
			}
		}
		return true
	})
	return out
}

// procTable is a snapshot of parent links for the processes we can see.
type procTable map[int]int // pid -> ppid

func readProcTable() procTable {
	t := procTable{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return t
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, _, ok := harness.ReadStat(pid); ok {
			t[pid] = ppid
		}
	}
	return t
}

// descendants returns root and every process below it.
func (t procTable) descendants(root int) []int {
	children := map[int][]int{}
	for pid, ppid := range t {
		children[ppid] = append(children[ppid], pid)
	}
	out := []int{root}
	for i := 0; i < len(out) && len(out) < 4096; i++ {
		out = append(out, children[out[i]]...)
	}
	return out
}

// attributeConn finds the thread whose harness tree holds the client end of a
// loopback TCP connection accepted on our side. ok is false when no harness
// tree holds it.
func (r *Runner) attributeConn(c net.Conn) (thread string, ok bool) {
	client, okc := c.RemoteAddr().(*net.TCPAddr)
	server, oks := c.LocalAddr().(*net.TCPAddr)
	if !okc || !oks {
		return "", false
	}
	inode, found := socketInode(client, server)
	if !found {
		return "", false
	}
	target := "socket:[" + inode + "]"

	procs := readProcTable()
	for _, h := range r.liveHarnesses() {
		// The PID must still be the process we started, not a reuse.
		if _, start, ok := harness.ReadStat(h.pid); !ok || start != h.start {
			continue
		}
		for _, pid := range procs.descendants(h.pid) {
			if holdsSocket(pid, target) {
				return h.thread, true
			}
		}
	}
	return "", false
}

// holdsSocket reports whether pid has the socket open.
func holdsSocket(pid int, target string) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if l, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && l == target {
			return true
		}
	}
	return false
}

// socketInode finds the inode of the TCP socket whose local end is client and
// remote end is server, in /proc/net/tcp. That is the client's socket: the
// one the calling process holds.
func socketInode(client, server *net.TCPAddr) (string, bool) {
	want := procAddr(client) + " " + procAddr(server)
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		// sl local rem st tx:rx tr:when retrnsmt uid timeout inode
		if len(f) < 10 {
			continue
		}
		if f[1]+" "+f[2] == want {
			return f[9], f[9] != "0"
		}
	}
	return "", false
}

// procAddr formats an IPv4 address the way /proc/net/tcp does: the address as
// a little-endian 32-bit word in hex, then the port in hex.
func procAddr(a *net.TCPAddr) string {
	ip := a.IP.To4()
	if ip == nil {
		return ""
	}
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", ip[3], ip[2], ip[1], ip[0], a.Port)
}
