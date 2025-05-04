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
	 tmp, err := os.CreateTemp("", "manifest-*.yaml")
	 if err != nil {
		 return err
	 }
	 defer os.Remove(tmp.Name())
 
	 if _, err := tmp.WriteString(manifest); err != nil {
		 tmp.Close()
		 return err
	 }
	 tmp.Close()
 
	 cmd := osExec.Command("kubectl", "apply", "-f", tmp.Name())
	 out, err := cmd.CombinedOutput()
	 if err != nil {
		 return fmt.Errorf("kubectl apply failed: %v\n%s", err, string(out))
	 }
	 return nil
 }
 
 // deleteResource ignores errors when deleting a Kubernetes resource.
 func deleteResource(kind, name string) {
	 osExec.Command("kubectl", "delete", kind, name, "--ignore-not-found").Run()
 }
 
 // getPodName returns the name of the first pod matching the label selector.
 func getPodName(namespace, labelSelector string) (string, error) {
	 cmd := osExec.Command("kubectl", "get", "pods", "-n", namespace,
		 "-l", labelSelector,
		 "-o", "jsonpath={.items[0].metadata.name}")
	 out, err := cmd.CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("failed to get pod: %v\n%s", err, string(out))
	 }
	 return string(out), nil
 }
 
 // getPodIP returns the IP of the first pod matching the label selector.
 func getPodIP(namespace, labelSelector string) (string, error) {
	 cmd := osExec.Command("kubectl", "get", "pods", "-n", namespace,
		 "-l", labelSelector,
		 "-o", "jsonpath={.items[0].status.podIP}")
	 out, err := cmd.CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("failed to get pod IP: %v\n%s", err, string(out))
	 }
	 return string(out), nil
 }
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 // 1) Enable XDP-based authorization
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("failed to enable XDP authz: %v\n%s", err, string(out))
	 }
 
	 // 2) Deploy Fortio server + service
	 serverDeploy := `
 apiVersion: apps/v1
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
		 args: ["server", "-port", "8080"]
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
	 if err := applyYAML(serverDeploy); err != nil {
		 t.Fatalf("deploy fortio-server failed: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-server")
	 defer deleteResource("service", "fortio-server")
 
	 // 3) Deploy Fortio client (sleeping pod)
	 clientDeploy := `
 apiVersion: apps/v1
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
	 if err := applyYAML(clientDeploy); err != nil {
		 t.Fatalf("deploy fortio-client failed: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-client")
 
	 // 4) Wait for pods to be Ready
	 if err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app=fortio-server",
		 "--timeout=120s").Run(); err != nil {
		 t.Fatalf("fortio-server not ready: %v", err)
	 }
	 if err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app=fortio-client",
		 "--timeout=120s").Run(); err != nil {
		 t.Fatalf("fortio-client not ready: %v", err)
	 }
 
	 // 5) Gather runtime info
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
 
	 // 6) Define test scenarios
	 tests := []struct {
		 name        string
		 policyName  string
		 manifest    string
		 target      string
		 logContains []string
	 }{
		 {
			 name:       "DenyByDstPort",
			 policyName: "deny-by-dstport",
			 manifest: fmt.Sprintf(`
 apiVersion: security.istio.io/v1beta1
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
		 ports: ["8080"]
 `),
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logContains: []string{"port 8080", "action: DENY"},
		 },
		 {
			 name:       "DenyBySrcIP",
			 policyName: "deny-by-srcip",
			 manifest: fmt.Sprintf(`
 apiVersion: security.istio.io/v1beta1
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
		 ipBlocks: ["%s"]
 `, clientIP),
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logContains: []string{"IPv4 match srcip", "action: DENY"},
		 },
		 {
			 name:       "DenyByDstIP",
			 policyName: "deny-by-dstip",
			 manifest: fmt.Sprintf(`
 apiVersion: security.istio.io/v1beta1
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
	   values: ["%s"]
 `, serverIP),
			 target:      fmt.Sprintf("%s:8080", serverIP),
			 logContains: []string{"IPv4 match dstip", "action: DENY"},
		 },
	 }
 
	 for _, tc := range tests {
		 tc := tc // capture range variable
		 t.Run(tc.name, func(t *testing.T) {
			 // Apply the policy
			 if err := applyYAML(tc.manifest); err != nil {
				 t.Fatalf("apply policy %s failed: %v", tc.policyName, err)
			 }
			 defer deleteResource("authorizationpolicy", tc.policyName)
 
			 // Give KMesh a moment to inject the policy
			 time.Sleep(3 * time.Second)
 
			 // Execute Fortio load; expect Code -1 (denied)
			 cmd := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", tc.target)
			 out, _ := cmd.CombinedOutput()
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("expected Fortio to be denied, got:\n%s", string(out))
			 }
 
			 // Verify KMesh XDP logs
			 kmeshPodBytes, err := osExec.Command("kubectl", "get", "pods", "-n", "kmesh-system",
				 "-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
			 if err != nil {
				 t.Fatalf("failed to list kmesh-system pod: %v\n%s", err, string(kmeshPodBytes))
			 }
			 kmeshPod := string(kmeshPodBytes)
 
			 logsBytes, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
			 logs := string(logsBytes)
 
			 for _, substr := range tc.logContains {
				 if !strings.Contains(logs, substr) {
					 t.Fatalf("expected kmesh logs to contain %q, got:\n%s", substr, logs)
				 }
			 }
		 })
	 }
 }
 