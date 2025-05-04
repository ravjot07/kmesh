//go:build integ
// +build integ

/*
 * Copyright The Kmesh Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

 package kmesh

 import (
	 "fmt"
	 "os"
	 osExec "os/exec"
	 "strings"
	 "testing"
	 "time"
 )
 
 // applyYAML writes the given manifest to a temp file and applies it via kubectl.
 func applyYAML(manifest string) error {
	 preview := manifest
	 if len(preview) > 200 {
		 preview = preview[:200] + "…"
	 }
	 fmt.Printf("DEBUG: Applying manifest preview:\n%s\n", preview)
 
	 tmp, err := os.CreateTemp("", "manifest-*.yaml")
	 if err != nil {
		 return fmt.Errorf("create temp file: %w", err)
	 }
	 defer os.Remove(tmp.Name())
 
	 if _, err := tmp.WriteString(manifest); err != nil {
		 tmp.Close()
		 return fmt.Errorf("write manifest: %w", err)
	 }
	 tmp.Close()
 
	 out, err := osExec.Command("kubectl", "apply", "-f", tmp.Name()).CombinedOutput()
	 if err != nil {
		 return fmt.Errorf("kubectl apply failed: %v\n%s", err, string(out))
	 }
	 return nil
 }
 
 // deleteResource ignores errors when deleting a Kubernetes resource.
 func deleteResource(kind, name string) {
	 fmt.Printf("DEBUG: Deleting resource %s/%s\n", kind, name)
	 osExec.Command("kubectl", "delete", kind, name, "--ignore-not-found").Run()
 }
 
 // getPodName returns the name of the first pod matching the label selector.
 func getPodName(namespace, labelSelector string) (string, error) {
	 fmt.Printf("DEBUG: Looking up pod name in namespace %s with selector %s\n", namespace, labelSelector)
	 out, err := osExec.Command("kubectl", "get", "pods", "-n", namespace,
		 "-l", labelSelector,
		 "-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("failed to get pod name: %v\n%s", err, string(out))
	 }
	 name := string(out)
	 fmt.Printf("DEBUG: Found pod name: %s\n", name)
	 return name, nil
 }
 
 // getPodIP returns the IP of the first pod matching the label selector.
 func getPodIP(namespace, labelSelector string) (string, error) {
	 fmt.Printf("DEBUG: Looking up pod IP in namespace %s with selector %s\n", namespace, labelSelector)
	 out, err := osExec.Command("kubectl", "get", "pods", "-n", namespace,
		 "-l", labelSelector,
		 "-o", "jsonpath={.items[0].status.podIP}").CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("failed to get pod IP: %v\n%s", err, string(out))
	 }
	 ip := string(out)
	 fmt.Printf("DEBUG: Found pod IP: %s\n", ip)
	 return ip, nil
 }
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 t.Log("=== TestXDPAuthorization: enabling XDP-based authz ===")
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("failed to enable XDP authz: %v\n%s", err, string(out))
	 }
 
	 // Deploy Fortio server + service
	 t.Log("=== Deploying Fortio server + service ===")
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
		 args: ["server","-port","8080"]
 ---
 apiVersion: v1
 kind: Service
 metadata:
   name: fortio-server
 spec:
   selector:
	 app: fortio-server
   ports:
   - port: 8080
	 targetPort: 8080
 `
	 if err := applyYAML(serverYAML); err != nil {
		 t.Fatalf("deploy fortio-server failed: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-server")
	 defer deleteResource("service", "fortio-server")
 
	 // Deploy Fortio client (sleeping pod)
	 t.Log("=== Deploying Fortio client ===")
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
		 command: ["sleep","3600"]
 `
	 if err := applyYAML(clientYAML); err != nil {
		 t.Fatalf("deploy fortio-client failed: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-client")
 
	 // Wait for pods to be Ready, with debug on failure
	 t.Log("Waiting for fortio-server to be Ready")
	 if err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app=fortio-server",
		 "--timeout=120s").Run(); err != nil {
 
		 // Debug info
		 podsList, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-o", "wide").CombinedOutput()
		 serverPodName, _ := getPodName(ns, "app=fortio-server")
		 describe, _ := osExec.Command("kubectl", "describe", "pod", serverPodName, "-n", ns).CombinedOutput()
		 logs, _ := osExec.Command("kubectl", "logs", serverPodName, "-n", ns).CombinedOutput()
		 t.Fatalf("fortio-server not ready: %v\n\nPods:\n%s\n\nDescribe %s:\n%s\n\nLogs %s:\n%s",
			 err, podsList, serverPodName, describe, serverPodName, logs)
	 }
 
	 t.Log("Waiting for fortio-client to be Ready")
	 if err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app=fortio-client",
		 "--timeout=120s").Run(); err != nil {
 
		 // Debug info
		 podsList, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-o", "wide").CombinedOutput()
		 clientPodName, _ := getPodName(ns, "app=fortio-client")
		 describe, _ := osExec.Command("kubectl", "describe", "pod", clientPodName, "-n", ns).CombinedOutput()
		 logs, _ := osExec.Command("kubectl", "logs", clientPodName, "-n", ns).CombinedOutput()
		 t.Fatalf("fortio-client not ready: %v\n\nPods:\n%s\n\nDescribe %s:\n%s\n\nLogs %s:\n%s",
			 err, podsList, clientPodName, describe, clientPodName, logs)
	 }
 
	 // Gather runtime info
	 clientPod, err := getPodName(ns, "app=fortio-client")
	 if err != nil {
		 t.Fatalf("could not get client pod: %v", err)
	 }
	 serverIP, err := getPodIP(ns, "app=fortio-server")
	 if err != nil {
		 t.Fatalf("could not get server IP: %v", err)
	 }
	 clientIP, err := getPodIP(ns, "app=fortio-client")
	 if err != nil {
		 t.Fatalf("could not get client IP: %v", err)
	 }
 
	 // Define test scenarios
	 tests := []struct {
		 name        string
		 policyName  string
		 manifest    string
		 target      string
		 logSnippets []string
	 }{
		 {
			 name:       "DenyByDstPort",
			 policyName: "deny-by-dstport",
			 manifest: `apiVersion: security.istio.io/v1beta1
 kind: AuthorizationPolicy
 metadata:
   name: deny-by-dstport
 spec:
   selector:
	 matchLabels:
	   app: fortio-server
   action: DENY
   rules:
   - to:
	 - operation:
		 ports: ["8080"]`,
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logSnippets: []string{"port 8080", "action: DENY"},
		 },
		 {
			 name:       "DenyBySrcIP",
			 policyName: "deny-by-srcip",
			 manifest: fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
 kind: AuthorizationPolicy
 metadata:
   name: deny-by-srcip
 spec:
   selector:
	 matchLabels:
	   app: fortio-server
   action: DENY
   rules:
   - from:
	 - source:
		 ipBlocks: ["%s"]`, clientIP),
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logSnippets: []string{"IPv4 match srcip", "action: DENY"},
		 },
		 {
			 name:       "DenyByDstIP",
			 policyName: "deny-by-dstip",
			 manifest: fmt.Sprintf(`apiVersion: security.istio.io/v1beta1
 kind: AuthorizationPolicy
 metadata:
   name: deny-by-dstip
 spec:
   selector:
	 matchLabels:
	   app: fortio-server
   action: DENY
   rules:
   - when:
	 - key: destination.ip
	   values: ["%s"]`, serverIP),
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logSnippets: []string{"IPv4 match dstip", "action: DENY"},
		 },
	 }
 
	 for _, tc := range tests {
		 tc := tc // capture range variable
		 t.Run(tc.name, func(t *testing.T) {
			 t.Logf("=== Scenario: %s ===", tc.name)
			 t.Logf("Applying policy %s:\n%s", tc.policyName, tc.manifest)
 
			 // Apply the policy
			 if err := applyYAML(tc.manifest); err != nil {
				 t.Fatalf("apply policy %s failed: %v", tc.policyName, err)
			 }
			 defer deleteResource("authorizationpolicy", tc.policyName)
 
			 // Allow propagation
			 t.Log("Sleeping 3s for policy propagation")
			 time.Sleep(3 * time.Second)
 
			 // Execute Fortio load; expect Code -1
			 cmdArgs := []string{"exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", tc.target}
			 t.Logf("Running Fortio: kubectl %s", strings.Join(cmdArgs, " "))
			 out, err := osExec.Command("kubectl", cmdArgs...).CombinedOutput()
			 output := string(out)
			 t.Logf("Fortio output:\n%s", output)
			 if err != nil {
				 t.Logf("Fortio returned error: %v", err)
			 }
			 if !strings.Contains(output, "Code -1") {
				 t.Fatalf("expected denied (Code -1), got:\n%s", output)
			 }
 
			 // Retrieve KMesh logs
			 t.Log("Retrieving KMesh daemon pod name")
			 podNameBytes, err := osExec.Command("kubectl", "get", "pods", "-n", "kmesh-system",
				 "-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
			 if err != nil {
				 t.Fatalf("get kmesh-system pod failed: %v\n%s", err, podNameBytes)
			 }
			 kmeshPod := string(podNameBytes)
			 t.Logf("KMesh daemon pod: %s", kmeshPod)
 
			 t.Log("Fetching KMesh logs")
			 logsBytes, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
			 logs := string(logsBytes)
			 t.Logf("KMesh logs preview:\n%s…", logs[:200])
 
			 for _, snippet := range tc.logSnippets {
				 t.Logf("Checking logs for %q", snippet)
				 if !strings.Contains(logs, snippet) {
					 t.Fatalf("logs missing %q", snippet)
				 }
			 }
		 })
	 }
 }