package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestContainerSmoke(t *testing.T) {
	image := os.Getenv("ARRTIFY_TEST_IMAGE")
	if image == "" {
		t.Skip("ARRTIFY_TEST_IMAGE is unset")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	helperDir := t.TempDir()
	helperBinary := filepath.Join(helperDir, "e2e-fixture")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", helperBinary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build static fixture helper: %v\n%s", err, output)
	}
	if err := os.Chmod(helperDir, 0755); err != nil {
		t.Fatal(err)
	}

	network := "arrtify-e2e-" + filepath.Base(t.TempDir())
	if _, err := runDocker(ctx, "network", "create", "--internal", network); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := runDocker(context.Background(), "network", "rm", network); err != nil {
			t.Logf("remove container smoke network: %v", err)
		}
	})

	config := configFile(t, "http://arrtify-fixture:18080", false)
	if err := os.Chmod(config, 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("offline check in locked-down image", func(t *testing.T) {
		result := recordContainerScenario(t, "offline check in locked-down image")
		args := append(dockerSecurityArgs(), "--network", "none", "--rm", "--mount", bindMount(config, "/config.yml"))
		for _, value := range cliEnv() {
			args = append(args, "--env", value)
		}
		args = append(args, image, "--config", "/config.yml", "--check")
		out, logs, exit := runDockerCLI(ctx, args...)
		result.Exit = exit
		result.Report = verifyOutput(t, out, logs, exit)
		if exit != 0 || result.Report.Status != "complete" || result.Report.Mode != "check" {
			t.Fatalf("exit %d, report %+v\n%s", exit, result.Report, logs)
		}
	})

	t.Run("dry-run reports queue guards without mutations", func(t *testing.T) {
		result := recordContainerScenario(t, "dry-run reports queue guards without mutations")
		fixtureID := startFixtureContainer(t, ctx, image, network, helperDir, "")
		out, logs, exit := runImageDryRun(ctx, image, network, config)
		requests, mutations := stopFixtureContainer(t, fixtureID)
		result.Exit = exit
		result.Requests = requests
		result.Mutations = mutations
		result.Report = verifyOutput(t, out, logs, exit)

		guarded := 0
		for _, decision := range result.Report.Decisions {
			if decision.Reason == "arr_queue" && decision.Outcome == "skipped" {
				guarded++
			}
		}
		if exit != 0 || result.Report.Status != "complete" || result.Report.WouldDelete == 0 || guarded < 2 {
			t.Fatalf("expected planned deletions and both Arr guards, got %+v", result.Report)
		}
		if requests == 0 || mutations != 0 || result.Report.Removed != 0 || result.Report.Attempted != 0 {
			t.Fatalf("fixture requests=%d mutations=%d, report %+v", requests, mutations, result.Report)
		}
	})

	t.Run("failed Arr queue stops the dry-run", func(t *testing.T) {
		result := recordContainerScenario(t, "failed Arr queue stops the dry-run")
		fixtureID := startFixtureContainer(t, ctx, image, network, helperDir, "sonarr")
		out, logs, exit := runImageDryRun(ctx, image, network, config)
		requests, mutations := stopFixtureContainer(t, fixtureID)
		result.Exit = exit
		result.Requests = requests
		result.Mutations = mutations
		result.Report = verifyOutput(t, out, logs, exit)

		if exit != 1 || result.Report.Status != "failed" || result.Report.Errors != 1 || result.Report.WouldDelete != 0 || result.Report.Removed != 0 || result.Report.Attempted != 0 || result.Report.NotAttempted != result.Report.Scanned {
			t.Fatalf("failed Arr queue did not stop decisions: %+v\n%s", result.Report, logs)
		}
		if requests == 0 || mutations != 0 {
			t.Fatalf("fixture requests=%d mutations=%d", requests, mutations)
		}
	})
}

func recordContainerScenario(t *testing.T, name string) *scenarioResult {
	t.Helper()
	result := &scenarioResult{Name: name, Exit: -1}
	t.Cleanup(func() {
		result.Passed = !t.Failed()
		containerEvidence = append(containerEvidence, *result)
	})
	return result
}

func startFixtureContainer(t *testing.T, ctx context.Context, image, network, helperDir, failService string) string {
	t.Helper()
	args := []string{
		"run", "--detach",
		"--network", network,
		"--network-alias", "arrtify-fixture",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65532:65532",
		"--env", "ARRTIFY_E2E_FIXTURE_SERVER=1",
		"--mount", bindMount(helperDir, "/fixture"),
		"--entrypoint", "/fixture/e2e-fixture",
	}
	if failService != "" {
		args = append(args, "--env", "ARRTIFY_E2E_FIXTURE_FAIL_SERVICE="+failService)
	}
	args = append(args, image)
	output, err := runDocker(ctx, args...)
	if err != nil {
		t.Fatalf("start fixture container: %v\n%s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		if _, err := runDocker(context.Background(), "rm", "--force", containerID); err != nil {
			t.Logf("remove fixture container: %v", err)
		}
	})
	waitForFixture(t, ctx, containerID)
	return containerID
}

func waitForFixture(t *testing.T, ctx context.Context, containerID string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		logs, _ := runDocker(ctx, "logs", containerID)
		if bytes.Contains(logs, []byte("ARRTIFY_FIXTURE_READY")) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fixture did not start before deadline: %v\n%s", ctx.Err(), logs)
		case <-deadline.C:
			t.Fatalf("fixture did not start\n%s", logs)
		case <-ticker.C:
		}
	}
}

func stopFixtureContainer(t *testing.T, containerID string) (int64, int64) {
	t.Helper()
	if output, err := runDocker(context.Background(), "stop", "--time", "5", containerID); err != nil {
		t.Fatalf("stop fixture container: %v\n%s", err, output)
	}
	output, err := runDocker(context.Background(), "logs", containerID)
	if err != nil {
		t.Fatalf("read fixture container logs: %v\n%s", err, output)
	}
	state, err := fixtureState(output)
	if err != nil {
		t.Fatalf("read fixture counters: %v\n%s", err, output)
	}
	return state.Requests, state.Mutations
}

func fixtureState(logs []byte) (scenarioResult, error) {
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	for scanner.Scan() {
		var state struct {
			Requests  int64 `json:"fixture_requests"`
			Mutations int64 `json:"fixture_mutations"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &state); err == nil {
			return scenarioResult{Requests: state.Requests, Mutations: state.Mutations}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return scenarioResult{}, err
	}
	return scenarioResult{}, errors.New("fixture state not found")
}

func runImageDryRun(ctx context.Context, image, network, config string) ([]byte, []byte, int) {
	args := append(dockerSecurityArgs(), "--network", network, "--rm", "--mount", bindMount(config, "/config.yml"))
	for _, value := range cliEnv() {
		args = append(args, "--env", value)
	}
	args = append(args, image, "--config", "/config.yml")
	return runDockerCLI(ctx, args...)
}

func dockerSecurityArgs() []string {
	return []string{"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532"}
}

func bindMount(source, destination string) string {
	return fmt.Sprintf("type=bind,source=%s,target=%s,readonly", source, destination)
}

func runDockerCLI(ctx context.Context, args ...string) ([]byte, []byte, int) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"run"}, args...)...)
	var out, logs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &logs
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.Bytes(), logs.Bytes(), exit.ExitCode()
		}
		return out.Bytes(), append(logs.Bytes(), []byte(err.Error())...), 125
	}
	return out.Bytes(), logs.Bytes(), 0
}

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.CombinedOutput()
}
