//go:build integ
// +build integ

/*
 * XDP-based L4 Authorization E2E test
 * - Deploy fortio server/client
 * - Apply deny-by-{dstPort,srcIP,dstIP} policies
 * - Expect fortio Code -1 + XDP log markers
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
 
 func applyYAML(manifest string) error {
	 tmp, err := os.CreateTemp("", "manifest-*.yaml")
	 if err != nil {
		 return err
	 }
	 defer os.Remove(tmp.Name())
 
	 if _, err := tmp.WriteString(strings.TrimSpace(manifest)); err != nil {
		 return err
	 }
	 _ = tmp.Close()
 
	 out, err := osExec.Command("kubectl", "apply", "-f", tmp.Name()).CombinedOutput()
	 if err != nil {
		 return fmt.Errorf("kubectl apply: %v\n%s", err, out)
	 }
	 return nil
 }
 
 func deleteResource(kind, name string) { _ = osExec.Command("kubectl", "delete", kind, name, "--ignore-not-found").Run() }
 
 func podName(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-l", sel, "-o",
		 "jsonpath={.items[0].metadata.name}").CombinedOutput()
	 return string(out)
 }
 func podIP(ns, sel string) string {
	 out, _ := osExec.Command("kubectl", "get", "pods", "-n", ns, "-l", sel, "-o",
		 "jsonpath={.items[0].status.podIP}").CombinedOutput()
	 return string(out)
 }
 
 func waitReady(ns, label string, t *testing.T) {
	 if err := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app="+label, "--timeout=120s").Run(); err != nil {
		 t.Fatalf("pod %s not Ready: %v", label, err)
	 }
 }
 
 func TestXDPAuthorization(t *testing.T) {
	 const ns = "default"
 
	 // Enable kernel-space authz
	 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
		 t.Fatalf("kmeshctl authz enable: %v\n%s", err, out)
	 }
 
	 // Fortio server & svc
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
		 t.Fatalf("deploy server: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-server")
	 defer deleteResource("service", "fortio-server")
 
	 // Fortio client
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
		 t.Fatalf("deploy client: %v", err)
	 }
	 defer deleteResource("deployment", "fortio-client")
 
	 waitReady(ns, "fortio-server", t)
	 waitReady(ns, "fortio-client", t)
 
	 clientPod := podName(ns, "app=fortio-client")
	 serverIP := podIP(ns, "app=fortio-server")
	 clientIP := podIP(ns, "app=fortio-client")
 
	 scenarios := []struct {
		 name   string
		 policy string
		 url    string
		 check  []string
	 }{
		 {
			 name: "deny-by-dstport",
			 policy: `
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
			 url:   fmt.Sprintf("%s:8080", serverIP),
			 check: []string{"port 8080", "action: DENY"},
		 },
		 {
			 name: "deny-by-srcip",
			 policy: fmt.Sprintf(`
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
		 ipBlocks: ["%s"]`, clientIP),
			 url:   fmt.Sprintf("%s:8080", serverIP),
			 check: []string{"srcip", "action: DENY"},
		 },
		 {
			 name: "deny-by-dstip",
			 policy: fmt.Sprintf(`
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
	   values: ["%s"]`, serverIP),
			 url:   fmt.Sprintf("%s:8080", serverIP),
			 check: []string{"dstip", "action: DENY"},
		 },
	 }
 
	 for _, sc := range scenarios {
		 sc := sc
		 t.Run(sc.name, func(t *testing.T) {
			 if err := applyYAML(sc.policy); err != nil {
				 t.Fatalf("apply policy: %v", err)
			 }
			 defer deleteResource("authorizationpolicy", sc.name)
 
			 time.Sleep(3 * time.Second)
 
			 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
				 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.url).CombinedOutput()
			 if !strings.Contains(string(out), "Code -1") {
				 t.Fatalf("traffic not denied – fortio:\n%s", out)
			 }
 
			 kmeshPod := podName("kmesh-system", "")
			 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod, "--tail=500").CombinedOutput()
			 for _, needle := range sc.check {
				 if !strings.Contains(string(logs), needle) {
					 t.Fatalf("log marker %q missing", needle)
				 }
			 }
		 })
	 }
 }
 