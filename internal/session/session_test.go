package session

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// mockTable creates a tableFn that returns the given entries.
func mockTable(entries map[int]processEntry) func() (map[int]processEntry, error) {
	return func() (map[int]processEntry, error) {
		return entries, nil
	}
}

// failingTable creates a tableFn that returns an error.
func failingTable() func() (map[int]processEntry, error) {
	return func() (map[int]processEntry, error) {
		return nil, fmt.Errorf("ps not found")
	}
}

func TestWalkToTopmostClaude_DirectParent(t *testing.T) {
	myPid := 100
	claudePid := 200
	table := map[int]processEntry{
		myPid:    {ppid: claudePid, comm: "mcp-proxy"},
		claudePid: {ppid: 500, comm: "claude"},
		500:      {ppid: 1, comm: "/sbin/launchd"},
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, claudePid, result)
}

func TestWalkToTopmostClaude_IntermediateChildProcess(t *testing.T) {
	// Two claude levels — topmost wins.
	myPid := 100
	childClaude := 200
	mainClaude := 300
	table := map[int]processEntry{
		myPid:       {ppid: childClaude, comm: "mcp-proxy"},
		childClaude: {ppid: mainClaude, comm: "claude"},
		mainClaude:  {ppid: 500, comm: "claude"},
		500:         {ppid: 1, comm: "/sbin/launchd"},
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, mainClaude, result)
}

func TestWalkToTopmostClaude_SingleClaudeWithShell(t *testing.T) {
	myPid := 100
	claudePid := 200
	shellPid := 300
	table := map[int]processEntry{
		myPid:    {ppid: claudePid, comm: "mcp-proxy"},
		claudePid: {ppid: shellPid, comm: "claude"},
		shellPid: {ppid: 1, comm: "-zsh"},
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, claudePid, result)
}

func TestWalkToTopmostClaude_MacOSFullPath(t *testing.T) {
	myPid := 100
	claudePid := 200
	table := map[int]processEntry{
		myPid:    {ppid: claudePid, comm: "mcp-proxy"},
		claudePid: {ppid: 500, comm: "/Applications/Claude.app/Contents/MacOS/claude"},
		500:      {ppid: 1, comm: "/sbin/launchd"},
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, claudePid, result)
}

func TestWalkToTopmostClaude_NoClaude(t *testing.T) {
	myPid := 100
	table := map[int]processEntry{
		myPid: {ppid: 500, comm: "mcp-proxy"},
		500:   {ppid: 1, comm: "zsh"},
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, os.Getppid(), result)
}

func TestWalkToTopmostClaude_PsFailure(t *testing.T) {
	result := walkToTopmostClaude(100, failingTable())
	assert.Equal(t, os.Getppid(), result)
}

func TestWalkToTopmostClaude_PidNotInTable(t *testing.T) {
	table := map[int]processEntry{
		999: {ppid: 1, comm: "init"},
	}
	result := walkToTopmostClaude(100, mockTable(table))
	assert.Equal(t, os.Getppid(), result)
}

func TestWalkToTopmostClaude_SafetyBound(t *testing.T) {
	// Chain of 15 non-claude processes — bounded to 10 iterations.
	myPid := 100
	table := make(map[int]processEntry)
	for i := range 15 {
		table[myPid+i] = processEntry{ppid: myPid + i + 1, comm: "python3"}
	}
	table[myPid+15] = processEntry{ppid: myPid + 15, comm: "python3"} // self-referential
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, os.Getppid(), result)
}

func TestWalkToTopmostClaude_CircularChain(t *testing.T) {
	myPid := 100
	table := map[int]processEntry{
		100: {ppid: 200, comm: "mcp-proxy"},
		200: {ppid: 300, comm: "bash"},
		300: {ppid: 100, comm: "zsh"}, // circular
	}
	result := walkToTopmostClaude(myPid, mockTable(table))
	assert.Equal(t, os.Getppid(), result)
}

func TestIsClaude(t *testing.T) {
	tests := []struct {
		comm string
		want bool
	}{
		{"claude", true},
		{"/usr/local/bin/claude", true},
		{"/Applications/Claude.app/Contents/MacOS/claude", true},
		{"python3", false},
		{"/bin/zsh", false},
		{"claude-helper", false},
		{"not-claude", false},
	}
	for _, tt := range tests {
		t.Run(tt.comm, func(t *testing.T) {
			assert.Equal(t, tt.want, isClaude(tt.comm))
		})
	}
}

func TestReadProcessTable_Integration(t *testing.T) {
	// Verify that readProcessTable() can actually parse ps output on this system.
	table, err := readProcessTable()
	if err != nil {
		t.Skip("ps not available:", err)
	}
	// We should at least find our own PID.
	myPid := os.Getpid()
	entry, ok := table[myPid]
	assert.True(t, ok, "current PID %d should be in process table", myPid)
	if ok {
		assert.Equal(t, os.Getppid(), entry.ppid)
	}
}
