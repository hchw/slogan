package laya

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{"..", ".."}, parts...)
	return filepath.Join(all...)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestLayaSidecarIsPrivateStaticCheck asserts the delivery artifacts never
// publish the sidecar on a public port and never carry a plaintext secret.
func TestLayaSidecarIsPrivateStaticCheck(t *testing.T) {
	compose := readFile(t, repoPath(t, "deploy", "docker-compose.yml"))
	start := strings.Index(compose, "\n  laya:")
	if start < 0 {
		t.Fatal("laya service missing from deploy/docker-compose.yml")
	}
	rest := compose[start+1:]
	end := strings.Index(rest, "\n  secrets:")
	if end < 0 {
		end = len(rest)
	}
	block := rest[:end]
	if !strings.Contains(block, "expose:") {
		t.Fatal("laya service must expose the port to the private network only")
	}
	if strings.Contains(block, "ports:") {
		t.Fatal("laya service must not publish a host port")
	}
	for _, want := range []string{"LAYA_API_KEY_FILE", "laya_api_key", "hf_models"} {
		if !strings.Contains(block, want) {
			t.Fatalf("laya service is missing %s", want)
		}
	}

	for _, name := range []string{"Dockerfile", "entrypoint.sh", "install.sh", "upgrade.sh", "preflight.sh", "smoke.sh", "status.sh", "rollback.sh", "compose.gpu.yml"} {
		body := readFile(t, repoPath(t, "deploy", "laya", name))
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, "LAYA_API_KEY=") && !strings.Contains(trimmed, "_FILE") && !strings.Contains(trimmed, "$(") && !strings.Contains(trimmed, "\"") {
				t.Fatalf("%s hardcodes a plaintext sidecar secret: %s", name, trimmed)
			}
		}
	}
}

func TestLayaScriptsParseAndDistinguishLivenessFromReadiness(t *testing.T) {
	for _, name := range []string{"entrypoint.sh", "install.sh", "upgrade.sh", "rollback.sh", "preflight.sh", "smoke.sh", "status.sh"} {
		path := repoPath(t, "deploy", "laya", name)
		if err := exec.Command("sh", "-n", path).Run(); err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
	}
	status := readFile(t, repoPath(t, "deploy", "laya", "status.sh"))
	for _, want := range []string{"model_loaded", "classifier_ready"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status.sh must report checkpoint readiness (%s), not just process liveness", want)
		}
	}
	install := readFile(t, repoPath(t, "deploy", "laya", "install.sh"))
	if !strings.Contains(install, "smoke.sh") || !strings.Contains(install, "preflight.sh") {
		t.Fatal("install.sh must run preflight and the typed-decision smoke test")
	}
	upgrade := readFile(t, repoPath(t, "deploy", "laya", "upgrade.sh"))
	if !strings.Contains(upgrade, "rollback.sh") {
		t.Fatal("upgrade.sh must keep the previous release serving on failure")
	}
}

func ensureSecret(t *testing.T) {
	t.Helper()
	path := repoPath(t, "deploy", "secrets", "laya_api_key.txt")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test-only-sidecar-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLayaPreflightCPUPathAndExplicitGPUFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ensureSecret(t)
	script := repoPath(t, "deploy", "laya", "preflight.sh")

	cpu := exec.Command("sh", script)
	cpu.Env = append(os.Environ(), "LAYA_MIN_CPU=1", "LAYA_MIN_MEM_MB=1", "LAYA_MIN_DISK_MB=1")
	out, err := cpu.CombinedOutput()
	if err != nil {
		t.Fatalf("CPU preflight should pass on a supported host: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "preflight ok") {
		t.Fatalf("CPU preflight output=%s", out)
	}
	note := strings.ToLower(string(out))
	if !strings.Contains(note, "cpu mode") {
		t.Fatalf("CPU preflight must state the CPU baseline explicitly: %s", out)
	}

	_, smiErr := exec.LookPath("nvidia-smi")
	gpu := exec.Command("sh", script, "--gpu")
	gpu.Env = append(os.Environ(), "LAYA_MIN_CPU=1", "LAYA_MIN_MEM_MB=1", "LAYA_MIN_DISK_MB=1")
	gpuOut, gpuErr := gpu.CombinedOutput()
	if smiErr != nil {
		// GPU unavailable: the request must fail loudly instead of degrading.
		if gpuErr == nil {
			t.Fatalf("--gpu without a GPU must fail explicitly, output=%s", gpuOut)
		}
		if !strings.Contains(string(gpuOut), "nvidia") {
			t.Fatalf("GPU failure message must name the missing driver/runtime: %s", gpuOut)
		}
	}
}
