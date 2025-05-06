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
// failing the test with full output on error.
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

// kubectlDelete deletes resources via stdin to kubectl delete.
// It logs errors but does not fail the test, ensuring best-effort cleanup.
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

// waitDeploymentReady waits up to 60s for the named Deployment to have at least 1 AvailableReplica.
// On failure, it gathers extra debug info (describe, pod list, pod logs) before failing.
func waitDeploymentReady(t *testing.T, name string) {
	t.Logf("DEBUG: Waiting for Deployment %q to become ready (60s timeout)...", name)

	// 1) Check rollout status
	rollCmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "--timeout=60s")
	if out, err := rollCmd.CombinedOutput(); err != nil {
		t.Logf("DEBUG: rollout status output:\n%s", out)

		// 2) Describe Deployment for events and conditions
		descOut, _ := exec.Command("kubectl", "describe", "deployment", name).CombinedOutput()
		t.Logf("DEBUG: describe deployment %q:\n%s", name, descOut)

		// 3) List Pods with their status
		podsOut, _ := exec.Command("kubectl", "get", "pods", "-l", "app="+name, "-o", "wide").CombinedOutput()
		t.Logf("DEBUG: pods for Deployment %q:\n%s", name, podsOut)

		// 4) Fetch logs from the first Pod
		podNameBytes, _ := exec.Command("kubectl", "get", "pods",
			"-l", "app="+name,
			"-o", "jsonpath={.items[0].metadata.name}").Output()
		podName := strings.TrimSpace(string(podNameBytes))
		if podName != "" {
			logsOut, _ := exec.Command("kubectl", "logs", podName).CombinedOutput()
			t.Logf("DEBUG: logs from Pod %q:\n%s", podName, logsOut)
		}

		// 5) Fail the test with the rollout error
		t.Fatalf("Deployment %q not ready in time: %v", name, err)
	}

	t.Logf("DEBUG: Deployment %q is now ready.", name)
}

// TestTCPAuthorizationXDP runs three AuthorizationPolicy scenarios:
//  1) deny-by-dstport
//  2) deny-by-srcip
//  3) deny-by-dstip
func TestTCPAuthorizationXDP(t *testing.T) {
	const namespace = "default"

	// 1) Deploy Fortio server
	serverYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: fortio-server
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
        args: ["server", "-http-port", "8078"]
        ports:
        - containerPort: 8078
`
	// 2) Service for Fortio server
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
	// 3) Deploy Fortio client
	clientYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: fortio-client
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
        image: fortio/fortio:latest
        command: ["sleep", "3600"]
`

	t.Log("DEBUG: Deploying Fortio server and client resources...")
	kubectlApply(t, serverYAML)
	kubectlApply(t, serviceYAML)
	kubectlApply(t, clientYAML)

	// Ensure cleanup at the end of the test
	defer kubectlDelete(t, clientYAML)
	defer kubectlDelete(t, serviceYAML)
	defer kubectlDelete(t, serverYAML)

	// Wait for deployments to become ready
	waitDeploymentReady(t, "fortio-server")
	waitDeploymentReady(t, "fortio-client")

	// Retrieve Pod IPs for use in policy templates
	clientIPBytes, err := exec.Command("kubectl", "get", "pod", "-l", "app=fortio-client",
		"-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to get fortio-client IP: %v", err)
	}
	serverIPBytes, err := exec.Command("kubectl", "get", "pod", "-l", "app=fortio-server",
		"-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to get fortio-server IP: %v", err)
	}
	clientIP := strings.TrimSpace(string(clientIPBytes))
	serverIP := strings.TrimSpace(string(serverIPBytes))
	t.Logf("DEBUG: fortio-client IP=%s, fortio-server IP=%s", clientIP, serverIP)

	// Define the AuthorizationPolicy manifests, injecting the Pod IPs
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

	// Helper to run a Fortio HTTP request from the client Pod
	runFortio := func(target string) (string, error) {
		// Fetch the client pod name
		podBytes, _ := exec.Command("kubectl", "get", "pods",
			"-l", "app=fortio-client",
			"-o", "jsonpath={.items[0].metadata.name}").Output()
		podName := strings.TrimSpace(string(podBytes))
		t.Logf("DEBUG: Executing Fortio load against %s from Pod %s", target, podName)

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
		time.Sleep(2 * time.Second) // allow policy propagation

		// Attempt request to service name (should be denied)
		output, err := runFortio("http://fortio-server.default:8078")
		t.Logf("DEBUG: Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Expected request to be denied, but it succeeded")
		}

		// Inspect Kmesh logs for the policy name
		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstport") {
			t.Errorf("Expected Kmesh logs to contain 'deny-by-dstport', got:\n%s", logs)
		}
	})

	// Scenario 2: deny-by-srcip
	t.Run("deny-by-srcip", func(t *testing.T) {
		t.Logf("DEBUG: Applying deny-by-srcip policy (source IP %s)...", clientIP)
		kubectlApply(t, policyBySrcIP)
		defer kubectlDelete(t, policyBySrcIP)
		time.Sleep(2 * time.Second)

		output, err := runFortio("http://fortio-server.default:8078")
		t.Logf("DEBUG: Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Expected request to be denied by source IP policy")
		}

		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-srcip") {
			t.Errorf("Expected Kmesh logs to contain 'deny-by-srcip', got:\n%s", logs)
		}
	})

	// Scenario 3: deny-by-dstip
	t.Run("deny-by-dstip", func(t *testing.T) {
		t.Logf("DEBUG: Applying deny-by-dstip policy (destination IP %s)...", serverIP)
		kubectlApply(t, policyByDstIP)
		defer kubectlDelete(t, policyByDstIP)
		time.Sleep(2 * time.Second)

		// Direct Pod IP request (should be denied)
		output, err := runFortio(fmt.Sprintf("http://%s:8078", serverIP))
		t.Logf("DEBUG: Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Expected request to Pod IP to be denied by dst IP policy")
		}

		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system",
			"-l", "app=kmesh", "--tail=50").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstip") {
			t.Errorf("Expected Kmesh logs to contain 'deny-by-dstip', got:\n%s", logs)
		}
	})
}