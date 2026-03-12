// Package session resolves the Claude Code session key from the process tree.
package session

import (
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
)

var (
	cachedKey  int
	cacheOnce  sync.Once
)

// FindSessionKey returns the PID of the topmost claude ancestor process.
// The result is cached for the lifetime of the process.
// Falls back to os.Getppid() if no claude ancestor is found or ps fails.
func FindSessionKey() int {
	cacheOnce.Do(func() {
		cachedKey = walkToTopmostClaude(os.Getpid(), readProcessTable)
	})
	return cachedKey
}

type processEntry struct {
	ppid int
	comm string
}

// walkToTopmostClaude walks upward from pid, returning the topmost claude ancestor.
// The tableFn parameter allows injection of a mock process table for testing.
func walkToTopmostClaude(pid int, tableFn func() (map[int]processEntry, error)) int {
	fallback := os.Getppid()

	table, err := tableFn()
	if err != nil {
		return fallback
	}

	topmostClaude := 0
	current := pid

	for range 10 { // safety bound — process trees are shallow
		entry, ok := table[current]
		if !ok {
			break
		}
		if isClaude(entry.comm) {
			topmostClaude = current
		}
		if entry.ppid == current || entry.ppid == 0 {
			break // reached init / root
		}
		current = entry.ppid
	}

	if topmostClaude != 0 {
		return topmostClaude
	}
	return fallback
}

// readProcessTable runs ps and parses output into {pid: (ppid, comm)}.
func readProcessTable() (map[int]processEntry, error) {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, err
	}

	table := make(map[int]processEntry)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// comm may contain spaces (unlikely for our targets but handle it).
		comm := strings.Join(fields[2:], " ")
		table[pid] = processEntry{ppid: ppid, comm: comm}
	}
	return table, nil
}

// isClaude checks whether a comm value refers to a Claude Code process.
// ps on macOS reports full paths; we match the basename.
func isClaude(comm string) bool {
	return path.Base(comm) == "claude"
}
