// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var completedEpicHeader = regexp.MustCompile(`^###\s+(EPIC-\d{4}).*✅\s+COMPLETED`)
var epicHeader = regexp.MustCompile(`^###\s+EPIC-\d{4}`)
var taskOrTCTestRow = regexp.MustCompile(`^\|\s*(T-\d{4}|TC-\d{4})\b`)

// TestCompletedEpicMarkerIntegrity ensures completed epic sections do not regress
// by leaving task/test rows without completion markers.
func TestCompletedEpicMarkerIntegrity(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	backlogPath := filepath.Join(repoRoot, "BACKLOG.md")
	b, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog: %v", err)
	}

	inCompletedEpic := false
	currentEpic := ""
	missing := make([]string, 0)

	for _, line := range strings.Split(string(b), "\n") {
		if m := completedEpicHeader.FindStringSubmatch(line); len(m) == 2 {
			inCompletedEpic = true
			currentEpic = m[1]
			continue
		}
		if epicHeader.MatchString(line) && !completedEpicHeader.MatchString(line) {
			inCompletedEpic = false
			currentEpic = ""
			continue
		}
		if inCompletedEpic && taskOrTCTestRow.MatchString(line) && !strings.Contains(line, "✅") {
			missing = append(missing, currentEpic+": "+strings.TrimSpace(line))
		}
	}

	if len(missing) > 0 {
		t.Fatalf("completed epic sections contain unmarked task/test rows: %s", strings.Join(missing, " | "))
	}
}
