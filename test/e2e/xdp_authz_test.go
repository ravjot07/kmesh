//go:build integ
// +build integ

/*
 * XDP-based L4 Authorization E2E test
 * Follows Istio’s Eval→Split→Apply pattern for YAML manifests.
 */

 package kmesh

 import (
	 "bytes"
	 "fmt"
	 osExec "os/exec"
	 "strings"
	 "testing"
	 "text/template"
	 "time"
 )
 
 // renderTemplate runs Go text/template on `tmpl`, using `params`,
 // then trims the leading newline and any common left margin.
 func renderTemplate(tmpl string, params any) (string, error) {
	 // 1) Execute the template
	 t := template.New("manifest").Option("missingkey=error")
	 t, err := t.Parse(tmpl)
	 if err != nil {
		 return "", err
	 }
	 var buf bytes.Buffer
	 if err := t.Execute(&buf, params); err != nil {
		 return "", err
	 }
	 out := buf.String()
 
	 // 2) Trim the first newline
	 out = strings.TrimPrefix(out, "\n")
 
	 // 3) Remove the common left margin
	 lines := strings.Split(out, "\n")
	 // find indent of first non-blank line
	 margin := -1
	 for _, l := range lines {
		 if strings.TrimSpace(l) == "" {
			 continue
		 }
		 indent := len(l) - len(strings.TrimLeft(l, " "))
		 margin = indent
		 break
	 }
	 if margin > 0 {
		 for i, l := range lines {
			 if len(l) >= margin {
				 lines[i] = l[margin:]
			 }
		 }
	 }
	 return strings.Join(lines, "\n"), nil
 }
 
 // applyDocs splits a multi-doc YAML (---) into individual docs and
 // streams each one to `kubectl apply -f -`.
 func applyDocs(yaml string) error {
	 docs := strings.Split(yaml, "\n---\n")
	 for _, d := range docs {
		 d = strings.TrimSpace(d)
		 if d == "" {
			 continue
		 }
		 cmd := osExec.Command("kubectl", "apply", "-f", "-")
		 cmd.Stdin = strings.NewReader(d)
		 if out, err := cmd.CombinedOutput(); err != nil {
			 return fmt.Errorf("kubectl apply: %v\n%s", err, out)
		 }
	 }
	 return nil
 }
 
 func deleteRes(kind, name, ns string) {
	 _ = osExec.Command("kubectl", "delete", kind, name, "-n", ns, "--ignore-not-found").Run()
 }
 
 func podName(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel,
		 "-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
	 return string(out)
 }
 
 func podIP(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel,
		 "-o", "jsonpath={.items[0].status.podIP}").CombinedOutput()
	 return string(out)
 }
 
 func waitReady(ns, label string, t *testing.T) {
	 if out, err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app="+label,
		 "--timeout=120s").CombinedOutput(); err != nil {
		 t.Fatalf("pod %s not Ready: %v\n%s", label, err, out)
	 }
 }
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 // Enable kernel-space authz
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("kmeshctl authz enable: %v\n%s", err, out)
	 }
 
	 // 1) Fortio server + Service
	 serverTmpl := `
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
	 rendered, err := renderTemplate(serverTmpl, nil)
	 if err != nil {
		 t.Fatalf("render server manifest: %v", err)
	 }
	 if err := applyDocs(rendered); err != nil {
		 t.Fatalf("apply server manifest: %v", err)
	 }
	 defer deleteRes("deployment", "fortio-server", ns)
	 defer deleteRes("service", "fortio-server", ns)
 
	 // 2) Fortio client
	 clientTmpl := `
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
	 rendered, err = renderTemplate(clientTmpl, nil)
	 if err != nil {
		 t.Fatalf("render client manifest: %v", err)
	 }
	 if err := applyDocs(rendered); err != nil {
		 t.Fatalf("apply client manifest: %v", err)
	 }
	 defer deleteRes("deployment", "fortio-client", ns)
 
	 waitReady(ns, "fortio-server", t)
	 waitReady(ns, "fortio-client", t)
 
	 clientPod := podName(ns, "app=fortio-client")
	 serverIP := podIP(ns, "app=fortio-server")
	 clientIP := podIP(ns, "app=fortio-client")
 
	 // 3) Scenarios
	 type scenario struct {
		 name    string
		 tmpl    string
		 params  any
		 target  string
		 logKeys []string
	 }
	 scenarios := []scenario{
		 {
			 name: "deny-by-dstport",
			 tmpl: `
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
		 ports: ["8080"]`,
			 params:  nil,
			 target:  fmt.Sprintf("%s:8080", serverIP),
			 logKeys: []string{"port 8080", "action: DENY"},
		 },
		 {
			 name: "deny-by-srcip",
			 tmpl: `
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
		 ipBlocks: ["{{.ClientIP}}"]`,
			 params:  map[string]string{"ClientIP": clientIP},
			 target:  fmt.Sprintf("%s:8080", serverIP),
			 logKeys: []string{"srcip", "action: DENY"},
		 },
		 {
			 name: "deny-by-dstip",
			 tmpl: `
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
	   values: ["{{.ServerIP}}"]`,
			 params:  map[string]string{"ServerIP": serverIP},
			 target:  fmt.Sprintf("%s:8080", serverIP),
			 logKeys: []string{"dstip", "action: DENY"},
		 },
	 }
 
	 for _, sc := range scenarios {
		 sc := sc
		 t.Run(sc.name, func(t *testing.T) {
			 rendered, err := renderTemplate(sc.tmpl, sc.params)
			 if err != nil {
				 t.Fatalf("render %s: %v", sc.name, err)
			 }
			 if err := applyDocs(rendered); err != nil {
				 t.Fatalf("apply %s: %v", sc.name, err)
			 }
			 defer deleteRes("authorizationpolicy", sc.name, ns)
 
			 time.Sleep(3 * time.Second) // propagation
 
			 // Fortio load → expect Code -1 (denied)
			 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.target).
				 CombinedOutput()
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("expected Code -1, got:\n%s", out)
			 }
 
			 // Check XDP logs
			 kmeshPod := podName("kmesh-system", "")
			 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
			 for _, key := range sc.logKeys {
				 if !strings.Contains(string(logs), key) {
					 t.Fatalf("logs missing %q", key)
				 }
			 }
		 })
	 }
 }
 