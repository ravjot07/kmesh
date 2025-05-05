//go:build integ
// +build integ

/*
 * XDP-based L4 Authorization E2E test with robust dedent + debug logging
 */

 package kmesh

 import (
	 "bytes"
	 "fmt"
	 "os"
	 osExec "os/exec"
	 "strings"
	 "testing"
	 "text/template"
	 "time"
 )
 
 // renderTemplate runs text/template on tmpl with params,
 // then:
 //  1) trims a leading newline
 //  2) replaces any leading TABs with two spaces
 //  3) finds the minimum indent (in spaces) across all non-blank lines
 //  4) removes exactly that indent from every line
 // This preserves nested structure while stripping uniform margin.
 func renderTemplate(tmpl string, params any) (string, error) {
	 // 1) Template execution
	 tpl := template.New("manifest").Option("missingkey=error")
	 tpl, err := tpl.Parse(tmpl)
	 if err != nil {
		 return "", err
	 }
	 var buf bytes.Buffer
	 if err := tpl.Execute(&buf, params); err != nil {
		 return "", err
	 }
	 out := buf.String()
 
	 // 2) Trim leading newline
	 out = strings.TrimPrefix(out, "\n")
 
	 // 3) Split into lines and normalize leading tabs→spaces
	 lines := strings.Split(out, "\n")
	 for i, l := range lines {
		 // extract leading whitespace (spaces or tabs)
		 j := 0
		 for j < len(l) && (l[j] == ' ' || l[j] == '\t') {
			 j++
		 }
		 prefix := l[:j]
		 // replace each tab in that prefix with two spaces
		 prefix = strings.ReplaceAll(prefix, "\t", "  ")
		 lines[i] = prefix + l[j:]
	 }
 
	 // 4) Compute minimum indent across non-blank lines
	 minIndent := -1
	 for _, l := range lines {
		 if strings.TrimSpace(l) == "" {
			 continue
		 }
		 // count leading spaces
		 indent := len(l) - len(strings.TrimLeft(l, " "))
		 if minIndent < 0 || indent < minIndent {
			 minIndent = indent
		 }
	 }
 
	 // 5) Strip that indent
	 if minIndent > 0 {
		 for i, l := range lines {
			 if len(l) >= minIndent {
				 lines[i] = l[minIndent:]
			 }
		 }
	 }
 
	 return strings.Join(lines, "\n"), nil
 }
 
 // applyDocs splits a multi-doc YAML on "\n---\n" and streams each to kubectl.
 // It prints DEBUG logs for both the document and the kubectl output.
 func applyDocs(yaml string) error {
	 docs := strings.Split(yaml, "\n---\n")
	 for idx, doc := range docs {
		 doc = strings.TrimSpace(doc)
		 if doc == "" {
			 continue
		 }
		 fmt.Printf("DEBUG: Applying document %d:\n%s\n---\n", idx+1, doc)
		 cmd := osExec.Command("kubectl", "apply", "-f", "-")
		 cmd.Stdin = strings.NewReader(doc)
		 out, err := cmd.CombinedOutput()
		 fmt.Printf("DEBUG: kubectl apply output:\n%s\n", out)
		 if err != nil {
			 return fmt.Errorf("kubectl apply failed on doc %d: %v", idx+1, err)
		 }
	 }
	 return nil
 }
 
 func deleteRes(kind, name, ns string) {
	 fmt.Printf("DEBUG: Deleting %s/%s in ns=%s\n", kind, name, ns)
	 _ = osExec.Command("kubectl", "delete", kind, name, "-n", ns, "--ignore-not-found").Run()
 }
 
 func podName(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel,
		 "-o", "jsonpath={.items[0].metadata.name}").
		 CombinedOutput()
	 return string(out)
 }
 
 func podIP(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns,
		 "-l", sel,
		 "-o", "jsonpath={.items[0].status.podIP}").
		 CombinedOutput()
	 return string(out)
 }
 
 func waitReady(ns, label string, t *testing.T) {
	 fmt.Printf("DEBUG: Waiting for pod app=%s in ns=%s to be Ready\n", label, ns)
	 out, err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app="+label,
		 "--timeout=120s").
		 CombinedOutput()
	 fmt.Printf("DEBUG: kubectl wait output:\n%s\n", out)
	 if err != nil {
		 t.Fatalf("pod %s not ready: %v", label, err)
	 }
 }
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 // Enable XDP authz
	 fmt.Println("DEBUG: Enabling XDP authz")
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("kmeshctl authz enable failed: %v\n%s", err, out)
	 } else {
		 fmt.Printf("DEBUG: kmeshctl authz enable output:\n%s\n", out)
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
	 // Render & debug
	 rendered, err := renderTemplate(serverTmpl, nil)
	 if err != nil {
		 t.Fatalf("render server manifest: %v", err)
	 }
	 fmt.Printf("DEBUG: Rendered server manifest:\n%s\n", rendered)
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
				 args: ["server"]
	 `
	 rendered, err = renderTemplate(clientTmpl, nil)
	 if err != nil {
		 t.Fatalf("render client manifest: %v", err)
	 }
	 fmt.Printf("DEBUG: Rendered client manifest:\n%s\n", rendered)
	 if err := applyDocs(rendered); err != nil {
		 t.Fatalf("apply client manifest: %v", err)
	 }
	 defer deleteRes("deployment", "fortio-client", ns)
 
	 waitReady(ns, "fortio-server", t)
	 waitReady(ns, "fortio-client", t)
 
	 clientPod := podName(ns, "app=fortio-client")
	 serverIP := podIP(ns, "app=fortio-server")
	 clientIP := podIP(ns, "app=fortio-client")
 
	 // 3) AuthorizationPolicy scenarios
	 type scenario struct {
		 name    string
		 tmpl    string
		 params  any
		 target  string
		 logKeys []string
	 }
	 scenarios := []scenario{
		 {
			 name:    "deny-by-dstport",
			 tmpl: `apiVersion: security.istio.io/v1beta1
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
			 name:    "deny-by-srcip",
			 tmpl: `apiVersion: security.istio.io/v1beta1
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
			 name:    "deny-by-dstip",
			 tmpl: `apiVersion: security.istio.io/v1beta1
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
			 fmt.Printf("DEBUG: Rendered %s policy:\n%s\n", sc.name, rendered)
			 if err := applyDocs(rendered); err != nil {
				 t.Fatalf("apply %s policy: %v", sc.name, err)
			 }
			 defer deleteRes("authorizationpolicy", sc.name, ns)
 
			 time.Sleep(3 * time.Second) // allow policy propagation
 
			 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.target).
				 CombinedOutput()
			 fmt.Printf("DEBUG: Fortio output for %s:\n%s\n", sc.name, out)
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("expected denial for %s, got:\n%s", sc.name, out)
			 }
 
			 kmeshPod := podName("kmesh-system", "")
			 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
			 fmt.Printf("DEBUG: KMesh logs for %s:\n%s\n", sc.name, logs)
			 for _, key := range sc.logKeys {
				 if !strings.Contains(string(logs), key) {
					 t.Fatalf("%s: missing log key %q", sc.name, key)
				 }
			 }
		 })
	 }
 }
 