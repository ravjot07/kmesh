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
 
 // dedent — removes the smallest common indentation **and** converts any
 // leading TAB characters to two spaces so the result is always YAML‑safe.
 func dedent(in string) string {
	 lines := strings.Split(in, "\n")
 
	 // 1) normalise the indentation – replace every tab at the start of a line
	 //    with two spaces (YAML forbids tab indents)
	 for i, l := range lines {
		 trim := strings.TrimLeft(l, " \t")
		 indentPart := l[:len(l)-len(trim)]
		 indentPart = strings.ReplaceAll(indentPart, "\t", "  ")
		 lines[i] = indentPart + trim
	 }
 
	 // 2) find smallest indent (in spaces) among non‑blank lines
	 min := -1
	 for _, l := range lines {
		 if strings.TrimSpace(l) == "" {
			 continue
		 }
		 n := len(l) - len(strings.TrimLeft(l, " "))
		 if min == -1 || n < min {
			 min = n
		 }
	 }
 
	 // 3) strip that indent
	 if min > 0 {
		 for i, l := range lines {
			 if len(l) >= min {
				 lines[i] = l[min:]
			 }
		 }
	 }
	 return strings.TrimSpace(strings.Join(lines, "\n"))
 }
 
 // applyYAML writes a manifest to a temp file (after dedenting/tabs→spaces)
 // and runs `kubectl apply -f`.
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
	 fmt.Printf("DEBUG: Deleting %s/%s\n", kind, name)
	 _ = osExec.Command("kubectl", "delete", kind, name, "--ignore-not-found").Run()
 }
 
 // getPodName returns the first pod name in a namespace, optionally filtered
 // by a label selector. If sel=="" we don't add the -l flag.
 func getPodName(ns, sel string) (string, error) {
	 args := []string{"get", "pods", "-n", ns}
	 if sel != "" {
		 args = append(args, "-l", sel)
	 }
	 args = append(args, "-o", "jsonpath={.items[0].metadata.name}")
	 out, err := osExec.Command("kubectl", args...).CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("kubectl get pods: %w\n%s", err, out)
	 }
	 return string(out), nil
 }
 
 // getPodIP returns the IP of the first pod matching a label selector.
 func getPodIP(ns, sel string) (string, error) {
	 out, err := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel, "-o", "jsonpath={.items[0].status.podIP}").CombinedOutput()
	 if err != nil {
		 return "", fmt.Errorf("kubectl get pod IP: %w\n%s", err, out)
	 }
	 return string(out), nil
 }
 
 ///////////////////////////////////////////////////////////////////////////////
 // E2E test
 ///////////////////////////////////////////////////////////////////////////////
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 //-----------------------------------------------------------------------
	 // Enable kernel‑space authz
	 //-----------------------------------------------------------------------
	 t.Log("Enabling XDP authz")
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("kmeshctl authz enable: %v\n%s", err, out)
	 }
 
	 //-----------------------------------------------------------------------
	 // Deploy Fortio server (port 8080) and service
	 //-----------------------------------------------------------------------
	 serverManifest := `
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
	 if err := applyYAML(serverManifest); err != nil {
		 t.Fatalf("deploy server: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-server")
	 defer deleteResource("service", "fortio-server")
 
	 //-----------------------------------------------------------------------
	 // Deploy Fortio client (sleep pod)
	 //-----------------------------------------------------------------------
	 clientManifest := `
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
	 if err := applyYAML(clientManifest); err != nil {
		 t.Fatalf("deploy client: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Wait for pods Ready (dump info on failure)
	 //-----------------------------------------------------------------------
	 waitReady := func(label string) {
		 if err := osExec.Command("kubectl", "wait", "-n", ns,
			 "--for=condition=Ready", "pod", "-l", "app="+label, "--timeout=120s").Run(); err != nil {
 
			 pods, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-o", "wide").CombinedOutput()
			 pod, _ := getPodName(ns, "app="+label)
			 desc, _ := osExec.Command("kubectl", "describe", "pod", pod, "-n", ns).CombinedOutput()
			 logs, _ := osExec.Command("kubectl", "logs", pod, "-n", ns, "--tail=100").CombinedOutput()
			 t.Fatalf("%s not ready: %v\nPods:\n%s\nDescribe:\n%s\nLogs:\n%s",
				 label, err, pods, desc, logs)
		 }
	 }
	 waitReady("fortio-server")
	 waitReady("fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Cluster info
	 //-----------------------------------------------------------------------
	 serverIP, _ := getPodIP(ns, "app=fortio-server")
	 clientIP, _ := getPodIP(ns, "app=fortio-client")
	 clientPod, _ := getPodName(ns, "app=fortio-client")
 
	 //-----------------------------------------------------------------------
	 // Scenarios table
	 //-----------------------------------------------------------------------
	 scenarios := []struct {
		 name       string
		 policyName string
		 manifest   string
		 targetURL  string
		 logNeedle  []string
	 }{
		 {
			 name:       "deny-by-dstport",
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
			 targetURL: fmt.Sprintf("%s:8080", serverIP),
			 logNeedle: []string{"port 8080", "action: DENY"},
		 },
		 {
			 name:       "deny-by-srcip",
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
			 targetURL: fmt.Sprintf("%s:8080", serverIP),
			 logNeedle: []string{"srcip", "action: DENY"},
		 },
		 {
			 name:       "deny-by-dstip",
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
			 targetURL: fmt.Sprintf("%s:8080", serverIP),
			 logNeedle: []string{"dstip", "action: DENY"},
		 },
	 }
 
	 //-----------------------------------------------------------------------
	 // Execute scenarios
	 //-----------------------------------------------------------------------
	 for _, sc := range scenarios {
		 sc := sc
		 t.Run(sc.name, func(t *testing.T) {
			 // Create policy
			 if err := applyYAML(sc.manifest); err != nil {
				 t.Fatalf("apply %s: %v", sc.policyName, err)
			 }
			 defer deleteResource("authorizationpolicy", sc.policyName)
 
			 time.Sleep(3 * time.Second) // let policy propagate
 
			 // Call server via Fortio – expect Code ‑1 (connection denied)
			 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.targetURL).CombinedOutput()
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("request not denied – fortio output:\n%s", out)
			 }
 
			 // Check KMesh daemon logs for rule match
			 kmeshPod, _ := getPodName("kmesh-system", "")
			 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod, "--tail=500").CombinedOutput()
			 for _, needle := range sc.logNeedle {
				 if !strings.Contains(string(logs), needle) {
					 t.Fatalf("log needle %q not found in KMesh logs", needle)
				 }
			 }
		 })
	 }
 }
 