package harness

import (
	"os"
	"strconv"
	"strings"
)

// ProcessStart returns a process's start time in clock ticks since boot, from
// /proc/<pid>/stat, or 0 where that is unavailable. Together with the PID it
// names one process for its whole life: PIDs are reused, start times are not.
func ProcessStart(pid int) uint64 {
	_, start, ok := ReadStat(pid)
	if !ok {
		return 0
	}
	return start
}

// ReadStat returns a process's parent PID and start time from /proc.
func ReadStat(pid int) (ppid int, start uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	// The command name is in parentheses and may itself contain spaces or
	// parentheses, so fields are counted from the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); ppid is field 4, starttime field 22.
	if len(f) < 20 {
		return 0, 0, false
	}
	pp, err1 := strconv.Atoi(f[1])
	st, err2 := strconv.ParseUint(f[19], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return pp, st, true
}
