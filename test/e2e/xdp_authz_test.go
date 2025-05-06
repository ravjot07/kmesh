//go:build integ
// +build integ

/*
 * XDP-based L4 Authorization E2E test with robust dedent + debug logging
 */

package kmesh

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kubectlApply applies a manifest via stdin to kubectl apply,
// logging both manifest and output, and failing the test on error.
func kubectlApply(t *testing.T, manifest string) {
	t.Logf("DEBUG: Applying manifest:\n%s\n---", manifest)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	t.Logf("DEBUG: kubectl apply output:\n%s", out)
	if err != nil {
		t.Fatalf("kubectl apply failed: %v\nOutput:\n%s", err, string(out))
	}
}

// kubectlDelete deletes resources defined in a manifest via stdin,
// logging the output but not failing the test if deletion errors.
func kubectlDelete(t *testing.T, manifest string) {
	t.Logf("DEBUG: Deleting resources defined in manifest...")
	cmd := exec.Command("kubectl", "delete", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	t.Logf("DEBUG: kubectl delete output:\n%s", out)
	if err != nil {
		t.Logf("WARN: kubectl delete encountered an error (ignored): %v", err)
	}
}

// waitDeploymentReady waits up to 60s for the named Deployment to have at least 1 available replica.
// On failure, it dumps describe, pod list, and pod logs for debugging.
func waitDeploymentReady(t *testing.T, name string) {
	t.Logf("DEBUG: Waiting for Deployment %q to become ready (60s timeout)...", name)
	rollCmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "--timeout=60s")
	if out, err := rollCmd.CombinedOutput(); err != nil {
		t.Logf("DEBUG: rollout status output:\n%s", out)

		// Describe the Deployment
		descOut, _ := exec.Command("kubectl", "describe", "deployment", name).CombinedOutput()
		t.Logf("DEBUG: describe deployment %q:\n%s", name, descOut)

		// List Pods
		podsOut, _ := exec.Command("kubectl", "get", "pods", "-l", "app="+name, "-o", "wide").CombinedOutput()
		t.Logf("DEBUG: pods for Deployment %q:\n%s", name, podsOut)

		// Tail logs from first Pod
		podNameBytes, _ := exec.Command("kubectl", "get", "pods",
			"-l", "app="+name,
			"-o", "jsonpath={.items[0].metadata.name}").Output()
		podName := strings.TrimSpace(string(podNameBytes))
		if podName != "" {
			logsOut, _ := exec.Command("kubectl", "logs", podName).CombinedOutput()
			t.Logf("DEBUG: logs from Pod %q:\n%s", podName, logsOut)
		}

		t.Fatalf("Deployment %q not ready in time: %v", name, err)
	}
	t.Logf("DEBUG: Deployment %q is now ready.", name)
}

func TestTCPAuthorizationXDP(t *testing.T) {
	const namespace = "default"

	// --- 1) Fortio server Deployment ---
	// Disable all but HTTP echo on port 8078 to avoid address-in-use errors.
	serverYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: fortio-server
  namespace: default
  labels:
    app: fortio-server
spec:
  replicas: 1
  selector:
    matchLabels:
      app: fortio-server
  template:
    metadata:
      labels:
        app: fortio-server
    spec:
      containers:
      - name: fortio-server
        image: fortio/fortio:latest
        args:
        - "server"
        - "-http-port"
        - "8078"
        - "-tcp-port"
        - "0"
        - "-udp-port"
        - "0"
        - "-grpc-port"
        - "0"
        ports:
        - containerPort: 8078
          protocol: TCP
`
	// --- 2) Fortio server Service ---
	serviceYAML := `apiVersion: v1
kind: Service
metadata:
  name: fortio-server
  namespace: default
spec:
  selector:
    app: fortio-server
  ports:
  - name: http
    protocol: TCP
    port: 8078
    targetPort: 8078
`
	// --- 3) Fortio client Deployment ---
	// Use busybox image so "sleep" is available.
	clientYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: fortio-client
  namespace: default
  labels:
    app: fortio-client
spec:
  replicas: 1
  selector:
    matchLabels:
      app: fortio-client
  template:
    metadata:
      labels:
        app: fortio-client
    spec:
      containers:
      - name: fortio-client
        image: busybox:1.35
        command: ["sleep", "3600"]
`

	t.Log("DEBUG: Deploying Fortio server and client resources...")
	kubectlApply(t, serverYAML)
	kubectlApply(t, serviceYAML)
	kubectlApply(t, clientYAML)

	// Ensure cleanup at end of test
	defer kubectlDelete(t, clientYAML)
	defer kubectlDelete(t, serviceYAML)
	defer kubectlDelete(t, serverYAML)

	// Wait for both Deployments to become ready
	waitDeploymentReady(t, "fortio-server")
	waitDeploymentReady(t, "fortio-client")

	// Fetch Pod IPs
	clientIPBytes, err := exec.Command("kubectl", "get", "pods",
		"-l", "app=fortio-client",
		"-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to retrieve fortio-client Pod IP: %v", err)
	}
	serverIPBytes, err := exec.Command("kubectl", "get", "pods",
		"-l", "app=fortio-server",
		"-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to retrieve fortio-server Pod IP: %v", err)
	}
	clientIP := strings.TrimSpace(string(clientIPBytes))
	serverIP := strings.TrimSpace(string(serverIPBytes))
	t.Logf("DEBUG: fortio-client IP=%s, fortio-server IP=%s", clientIP, serverIP)

	// --- AuthorizationPolicy manifests ---
	policyByDstPort := `apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-by-dstport
  namespace: default
spec:
  action: DENY
  rules:
  - to:
    - operation:
        ports: ["8078"]
`
	policyBySrcIP := fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-by-srcip
  namespace: default
spec:
  action: DENY
  rules:
  - from:
    - source:
        ipBlocks: ["%s/32"]
`, clientIP)
	policyByDstIP := fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: deny-by-dstip
  namespace: default
spec:
  action: DENY
  rules:
  - to:
    - destination:
        ipBlocks: ["%s/32"]
`, serverIP)

	// Helper to run a single Fortio HTTP request from the client Pod
	runFortio := func(target string) (string, error) {
		podNameBytes, _ := exec.Command("kubectl", "get", "pods",
			"-l", "app=fortio-client",
			"-o", "jsonpath={.items[0].metadata.name}").Output()
		podName := strings.TrimSpace(string(podNameBytes))
		t.Logf("DEBUG: Fortio load → %s from pod %s", target, podName)
		cmd := exec.Command("kubectl", "exec", podName, "--",
			"fortio", "load", "-qps", "0", "-n", "1", "-timeout", "5s", target)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Scenario 1: deny-by-dstport
	t.Run("deny-by-dstport", func(t *testing.T) {
		t.Log("DEBUG: Applying deny-by-dstport policy...")
		kubectlApply(t, policyByDstPort)
		defer kubectlDelete(t, policyByDstPort)
		time.Sleep(2 * time.Second)

		out, err := runFortio("http://fortio-server.default:8078")
		t.Logf("DEBUG: Fortio output:\n%s", out)
		if err == nil {
			t.Errorf("Expected denial by dstport but request succeeded")
		}

		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstport") {
			t.Errorf("Kmesh logs missing 'deny-by-dstport':\n%s", logs)
		}
	})

	// Scenario 2: deny-by-srcip
	t.Run("deny-by-srcip", func(t *testing.T) {
		t.Logf("DEBUG: Applying deny-by-srcip (client IP=%s)...", clientIP)
		kubectlApply(t, policyBySrcIP)
		defer kubectlDelete(t, policyBySrcIP)
		time.Sleep(2 * time.Second)

		out, err := runFortio("http://fortio-server.default:8078")
		t.Logf("DEBUG: Fortio output:\n%s", out)
		if err == nil {
			t.Errorf("Expected denial by srcip but request succeeded")
		}

		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-srcip") {
			t.Errorf("Kmesh logs missing 'deny-by-srcip':\n%s", logs)
		}
	})

	// Scenario 3: deny-by-dstip
	t.Run("deny-by-dstip", func(t *testing.T) {
		t.Logf("DEBUG: Applying deny-by-dstip (server IP=%s)...", serverIP)
		kubectlApply(t, policyByDstIP)
		defer kubectlDelete(t, policyByDstIP)
		time.Sleep(2 * time.Second)

		out, err := runFortio(fmt.Sprintf("http://%s:8078", serverIP))
		t.Logf("DEBUG: Fortio output:\n%s", out)
		if err == nil {
			t.Errorf("Expected denial by dstip but request succeeded")
		}

		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstip") {
			t.Errorf("Kmesh logs missing 'deny-by-dstip':\n%s", logs)
		}
	})
}