package main

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHardenedDeploymentContainerServes runs the pinned MinIO image under the
// exact restricted securityContext the deployment manifest applies — read-only
// root filesystem, non-root UID/GID, every capability dropped, no privilege
// escalation, and only /tmp plus the data dir writable — and verifies the
// server still starts and answers its health probe. Both output profiles render
// this hardening, so a securityContext or image change that left MinIO unable to
// run would crash-loop every rendered pod; the static manifest contract cannot
// catch that because it never executes the image.
func TestHardenedDeploymentContainerServes(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	const name = "minio-hardened-test"
	_ = exec.Command("docker", "rm", "-f", name).Run()

	run := exec.Command("docker", "run", "-d", "--name", name,
		"--read-only",
		"--user", "1000:1000",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:uid=1000,gid=1000",
		"--tmpfs", "/data:uid=1000,gid=1000",
		"-e", "MINIO_ACCESS_KEY=minio",
		"-e", "MINIO_SECRET_KEY=miniopassword",
		"-p", "127.0.0.1::9000",
		image.FullName(), "server", "/data",
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run hardened MinIO: %v: %s", err, out)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	address := hostAddress(t, name)
	healthURL := "http://" + address + "/minio/health/live"

	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(45 * time.Second)
	for {
		resp, err := client.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("hardened MinIO never became healthy at %s: last err %v\nlogs:\n%s", healthURL, err, logs)
		}
		time.Sleep(time.Second)
	}
}

// hostAddress resolves the host-side "127.0.0.1:port" mapping docker assigned to
// the container's MinIO API port.
func hostAddress(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "port", container, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	mapping := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if mapping == "" {
		t.Fatalf("no host mapping for container port 9000: %q", out)
	}
	return mapping
}

func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}
