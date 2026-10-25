package e2e

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run a real failing scenario in a disposable source checkout. Its artifact must
// record the failure even though Fatal stops the scenario before its final line.
func TestFailedScenarioIsRecorded(t *testing.T) {
	workspace := t.TempDir()
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join("../..", dir), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel("../..", path)
			if err != nil {
				return err
			}
			destination := filepath.Join(workspace, relative)
			if entry.IsDir() {
				return os.MkdirAll(destination, 0755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, data, 0644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"go.mod", "go.sum", "config.example.yml"} {
		data, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "config.example.yml" {
			data = append(data, []byte("unexpected: failure-probe\n")...)
		}
		if err := os.WriteFile(filepath.Join(workspace, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	testDir := filepath.Join(workspace, "tests", "e2e")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBinary, "-test.run=^TestCLI/dry-run_protects_both_full_queues$")
	cmd.Dir = testDir
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected failed scenario: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "artifacts", "e2e.json"))
	if err != nil {
		t.Fatal(err)
	}
	results := []scenarioResult{}
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("failed scenario missing from artifact: got %d records\n%s", len(results), output)
	}
	result := results[0]
	if result.Name != "dry-run protects both full queues" || result.Passed || result.Exit != 2 || result.Requests != 0 || result.Mutations != 0 || result.Report.Status != "failed" {
		t.Fatalf("incorrect failure evidence: %+v", result)
	}
}
