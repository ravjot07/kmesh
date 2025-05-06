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

// Helper to apply a Kubernetes manifest via kubectl
func kubectlApply(t *testing.T, manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to apply manifest: %v\nOutput: %s", err, string(out))
	}
}

// Helper to delete resources defined in a manifest via kubectl
func kubectlDelete(t *testing.T, manifest string) {
	cmd := exec.Command("kubectl", "delete", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_ = cmd.Run() // no need to fail test if cleanup fails
}

// Helper to wait for a deployment to be ready
func waitDeploymentReady(t *testing.T, name string) {
	cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "--timeout=60s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Deployment %s not ready in time: %v\nOutput: %s", name, err, string(out))
	}
}

func TestTCPAuthorizationXDP(t *testing.T) {
	// Define YAML manifests for Fortio server deployment, service, and client deployment
	serverDeploymentYAML := `apiVersion: apps/v1
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
        args: ["server", "-grpc-port", "0", "-udp-port", "0", "-http-port", "8078"]
        ports:
        - containerPort: 8078
          protocol: TCP
`
	serviceYAML := `apiVersion: v1
kind: Service
metadata:
  name: fortio-server
spec:
  selector:
    app: fortio-server
  ports:
  - name: http
    protocol: TCP
    port: 8078
    targetPort: 8078
`
	clientDeploymentYAML := `apiVersion: apps/v1
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
        command: ["/bin/bash", "-c", "sleep 3600"]
`

	// Apply Fortio server deployment and service, and Fortio client deployment
	t.Log("Deploying Fortio server and client workloads...")
	kubectlApply(t, serverDeploymentYAML)
	kubectlApply(t, serviceYAML)
	kubectlApply(t, clientDeploymentYAML)
	// Ensure cleanup at the end
	defer kubectlDelete(t, clientDeploymentYAML)
	defer kubectlDelete(t, serviceYAML)
	defer kubectlDelete(t, serverDeploymentYAML)

	// Wait for fortio-server and fortio-client pods to be ready
	waitDeploymentReady(t, "fortio-server")
	waitDeploymentReady(t, "fortio-client")

	// Retrieve the Fortio client and server Pod IP addresses for policy rules
	clientIPBytes, err := exec.Command("kubectl", "get", "pod", "-l", "app=fortio-client", "-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to get fortio-client Pod IP: %v", err)
	}
	serverIPBytes, err := exec.Command("kubectl", "get", "pod", "-l", "app=fortio-server", "-o", "jsonpath={.items[0].status.podIP}").Output()
	if err != nil {
		t.Fatalf("Failed to get fortio-server Pod IP: %v", err)
	}
	clientIP := strings.TrimSpace(string(clientIPBytes))
	serverIP := strings.TrimSpace(string(serverIPBytes))
	t.Logf("Fortio client Pod IP: %s, server Pod IP: %s", clientIP, serverIP)

	// Define AuthorizationPolicy YAMLs for each scenario, inserting the retrieved IPs
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

	// Helper function to run a Fortio request from the client pod and return output and error
	runFortioFromClient := func(target string) (string, error) {
		// Get the client pod name
		podNameBytes, err := exec.Command("kubectl", "get", "pods", "-l", "app=fortio-client", "-o", "jsonpath={.items[0].metadata.name}").Output()
		if err != nil {
			return "", fmt.Errorf("failed to get fortio-client pod name: %v", err)
		}
		podName := strings.TrimSpace(string(podNameBytes))
		// Execute a single Fortio request to the target from the client pod
		cmd := exec.Command("kubectl", "exec", podName, "--", "fortio", "load", "-qps", "0", "-n", "1", "-timeout", "5s", target)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	// Scenario 1: Deny by destination port
	t.Run("deny-by-dstport", func(t *testing.T) {
		t.Log("Applying deny-by-dstport policy (deny traffic to port 8078)...")
		kubectlApply(t, policyByDstPort)
		defer kubectlDelete(t, policyByDstPort)
		// Give a moment for policy to propagate
		time.Sleep(2 * time.Second)

		// Attempt to connect from fortio-client to fortio-server service (port 8078 should be denied)
		targetURL := fmt.Sprintf("http://fortio-server.default:8078")
		output, err := runFortioFromClient(targetURL)
		t.Logf("Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Fortio client request unexpectedly succeeded (expected denial)")
		}

		// Check Kmesh logs for evidence of denial by this policy
		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system", "-l", "app=kmesh", "--tail=100").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstport") {
			t.Errorf("Kmesh logs do not contain expected deny-by-dstport entry")
		}
	})

	// Scenario 2: Deny by source IP
	t.Run("deny-by-srcip", func(t *testing.T) {
		t.Logf("Applying deny-by-srcip policy (deny traffic from source IP %s)...", clientIP)
		kubectlApply(t, policyBySrcIP)
		defer kubectlDelete(t, policyBySrcIP)
		time.Sleep(2 * time.Second)

		// Attempt to connect from fortio-client to fortio-server (client IP is denied)
		targetURL := fmt.Sprintf("http://fortio-server.default:8078")
		output, err := runFortioFromClient(targetURL)
		t.Logf("Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Fortio client request unexpectedly succeeded (expected denial)")
		}

		// Verify Kmesh logs contain policy name
		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system", "-l", "app=kmesh", "--tail=100").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-srcip") {
			t.Errorf("Kmesh logs do not contain expected deny-by-srcip entry")
		}
	})

	// Scenario 3: Deny by destination IP
	t.Run("deny-by-dstip", func(t *testing.T) {
		t.Logf("Applying deny-by-dstip policy (deny traffic to destination IP %s)...", serverIP)
		kubectlApply(t, policyByDstIP)
		defer kubectlDelete(t, policyByDstIP)
		time.Sleep(2 * time.Second)

		// Attempt to connect from fortio-client directly to server Pod IP on port 8078
		targetURL := fmt.Sprintf("http://%s:8078", serverIP)
		output, err := runFortioFromClient(targetURL)
		t.Logf("Fortio output:\n%s", output)
		if err == nil {
			t.Errorf("Fortio client request unexpectedly succeeded (expected denial)")
		}

		// Check Kmesh logs for denial entry
		logs, _ := exec.Command("kubectl", "logs", "-n", "kmesh-system", "-l", "app=kmesh", "--tail=100").CombinedOutput()
		if !strings.Contains(string(logs), "deny-by-dstip") {
			t.Errorf("Kmesh logs do not contain expected deny-by-dstip entry")
		}
	})
}