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
 
 ///////////////////////////////////////////////////////////////////////////////
 // Helper utilities
 ///////////////////////////////////////////////////////////////////////////////
 
 // dedent removes the smallest common indentation from all non‑blank lines.
 // It lets us embed nicely indented raw‑string YAML without breaking kubectl.
 func dedent(in string) string {
	 lines := strings.Split(in, "\n")
 
	 // Find minimum indent among non‑blank lines.
	 min := -1
	 for _, l := range lines {
		 if strings.TrimSpace(l) == "" {
			 continue
		 }
		 indent := len(l) - len(strings.TrimLeft(l, " \t"))
		 if min == -1 || indent < min {
			 min = indent
		 }
	 }
	 if min > 0 {
		 for i, l := range lines {
			 if len(l) >= min {
				 lines[i] = l[min:]
			 }
		 }
	 }
	 return strings.TrimSpace(strings.Join(lines, "\n"))
 }
 
 // applyYAML writes manifest to a temp file (after dedenting) and runs kubectl.
 func applyYAML(manifest string) error {
	 manifest = dedent(manifest)
 
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
		 return fmt.Errorf("kubectl apply failed: %v\n%s", err, out)
	 }
	 return nil
 }
 
 // deleteResource ignores errors when deleting a Kubernetes resource.
 func deleteResource(kind, name string) {
	 fmt.Printf("DEBUG: Deleting resource %s/%s\n", kind, name)
	 osExec.Command("kubectl", "delete", kind, name, "--ignore-not-found").Run()
 }
 
 // getPodName returns the first pod name matching a selector.
 func getPodName(ns, sel string) (string, error) {
	 out, err := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel, "-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("get pod name: %w\n%s", err, out)
	 }
	 return string(out), nil
 }
 
 // getPodIP returns the IP of the first pod matching a selector.
 func getPodIP(ns, sel string) (string, error) {
	 out, err := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel, "-o", "jsonpath={.items[0].status.podIP}").CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("get pod IP: %w\n%s", err, out)
	 }
	 return string(out), nil
 }
 
 ///////////////////////////////////////////////////////////////////////////////
 // E2E test
 ///////////////////////////////////////////////////////////////////////////////
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 //-----------------------------------------------------------------------
	 // Enable XDP‑based authorization
	 //-----------------------------------------------------------------------
	 t.Log("Enabling XDP‑based authorization (kmeshctl authz enable)")
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("kmeshctl authz enable failed: %v\n%s", err, out)
	 }
 
	 //-----------------------------------------------------------------------
	 // Deploy Fortio server (8080) and service
	 //-----------------------------------------------------------------------
	 serverYAML := `
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
		 t.Fatalf("deploy fortio-server: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-server")
	 defer deleteResource("service", "fortio-server")
 
	 //-----------------------------------------------------------------------
	 // Deploy Fortio client (sleep pod)
	 //-----------------------------------------------------------------------
	 clientYAML := `
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
			 command: ["sleep","3600"]
	 `
	 if err := applyYAML(clientYAML); err != nil {
		 t.Fatalf("deploy fortio-client: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Wait for pods to be ready (with debug on failure)
	 //-----------------------------------------------------------------------
	 waitReady := func(label string) {
		 if err := osExec.Command("kubectl", "wait", "-n", ns,
			 "--for=condition=Ready", "pod", "-l", "app="+label, "--timeout=120s").Run(); err != nil {
 
			 // dump info
			 pods, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-o", "wide").CombinedOutput()
			 pod, _ := getPodName(ns, "app="+label)
			 desc, _ := osExec.Command("kubectl", "describe", "pod", pod, "-n", ns).CombinedOutput()
			 logs, _ := osExec.Command("kubectl", "logs", pod, "-n", ns).CombinedOutput()
			 t.Fatalf("%s not Ready: %v\n\nPods:\n%s\n\ndescribe:\n%s\n\nlogs:\n%s", label, err, pods, desc, logs)
		 }
	 }
	 waitReady("fortio-server")
	 waitReady("fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Runtime info
	 //-----------------------------------------------------------------------
	 clientPod, _ := getPodName(ns, "app=fortio-client")
	 serverIP, _ := getPodIP(ns, "app=fortio-server")
	 clientIP, _ := getPodIP(ns, "app=fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Scenarios
	 //-----------------------------------------------------------------------
	 scenarios := []struct {
		 name        string
		 policyName  string
		 manifest    string
		 target      string
		 logMarkers  []string
	 }{
		 {
			 name:       "DenyByDstPort",
			 policyName: "deny-by-dstport",
			 manifest: `
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
			 `,
			 target:     fmt.Sprintf("%s:8080", serverIP),
			 logMarkers: []string{"port 8080", "action: DENY"},
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
			 target:     fmt.Sprintf("%s:8080", serverIP),
			 logMarkers: []string{"srcip", "action: DENY"},
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
			 target:     fmt.Sprintf("%s:8080", serverIP),
			 logMarkers: []string{"dstip", "action: DENY"},
		 },
	 }
 
	 //-----------------------------------------------------------------------
	 // Execute each scenario
	 //-----------------------------------------------------------------------
	 for _, sc := range scenarios {
		 sc := sc // capture
		 t.Run(sc.name, func(t *testing.T) {
			 // Apply policy
			 if err := applyYAML(sc.manifest); err != nil {
				 t.Fatalf("apply %s: %v", sc.policyName, err)
			 }
			 defer deleteResource("authorizationpolicy", sc.policyName)
 
			 time.Sleep(3 * time.Second) // propagation
 
			 // Run Fortio (expect Code -1)
			 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.target).CombinedOutput()
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("traffic not denied, fortio output:\n%s", out)
			 }
 
			 // Check KMesh logs
			 kmeshPod, _ := getPodName("kmesh-system", "")
			 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
			 for _, m := range sc.logMarkers {
				 if !strings.Contains(string(logs), m) {
					 t.Fatalf("log marker %q not found in KMesh logs", m)
				 }
			 }
		 })
	 }
 }
 