//go:build integ
// +build integ

/*
 * XDP-based L4 Authorization E2E test, using Istio test-framework Eval/Split/Apply
 */

 package kmesh

 import (
	 "fmt"
	 osExec "os/exec"
	 "strings"
	 "testing"
	 "time"
 
	 "istio.io/istio/pkg/test/framework"
	 istioComponent "istio.io/istio/pkg/test/framework/components/istio"
 )
 
 func waitReady(ns, label string, t framework.TestContext) {
	 cmd := osExec.Command("kubectl", "wait", "-n", ns,
		 "--for=condition=Ready", "pod", "-l", "app="+label,
		 "--timeout=120s")
	 if out, err := cmd.CombinedOutput(); err != nil {
		 t.Fatalf("pod %s not Ready: %v\n%s", label, err, out)
	 }
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
 
 func TestXDPAuthorization(t *testing.T) {
	 framework.NewTest(t).Run(func(ctx framework.TestContext) {
		 const ns = "default"
 
		 // 1) Enable XDP authz in kernel
		 if out, err := osExec.Command("kmeshctl", "authz", "enable").CombinedOutput(); err != nil {
			 ctx.Fatalf("kmeshctl authz enable failed: %v\n%s", err, out)
		 }
 
		 // 2) Deploy Fortio server + svc via Eval/Split/Apply
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
		 istioComponent.ConfigIstio().Eval(ns, nil, serverYAML).ApplyOrFail(ctx)
		 defer osExec.Command("kubectl", "delete", "deployment", "fortio-server", "-n", ns, "--ignore-not-found").Run()
		 defer osExec.Command("kubectl", "delete", "service", "fortio-server", "-n", ns, "--ignore-not-found").Run()
 
		 // 3) Deploy Fortio client (sleep pod)
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
		 istioComponent.ConfigIstio().Eval(ns, nil, clientYAML).ApplyOrFail(ctx)
		 defer osExec.Command("kubectl", "delete", "deployment", "fortio-client", "-n", ns, "--ignore-not-found").Run()
 
		 // 4) Wait for pods
		 waitReady(ns, "fortio-server", ctx)
		 waitReady(ns, "fortio-client", ctx)
 
		 // 5) Gather runtime info
		 clientPod := podName(ns, "app=fortio-client")
		 serverIP := podIP(ns, "app=fortio-server")
		 clientIP := podIP(ns, "app=fortio-client")
 
		 // 6) Scenarios
		 scenarios := []struct {
			 name      string
			 template  string
			 params    map[string]string
			 target    string
			 logChecks []string
		 }{
			 {
				 name: "deny-by-dstport",
				 template: `
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
				 params:    nil,
				 target:    fmt.Sprintf("%s:8080", serverIP),
				 logChecks: []string{"port 8080", "action: DENY"},
			 },
			 {
				 name: "deny-by-srcip",
				 template: `
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
		 ipBlocks: ["{{.ClientIP}}"]
 `,
				 params:    map[string]string{"ClientIP": clientIP},
				 target:    fmt.Sprintf("%s:8080", serverIP),
				 logChecks: []string{"srcip", "action: DENY"},
			 },
			 {
				 name: "deny-by-dstip",
				 template: `
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
	   values: ["{{.ServerIP}}"]
 `,
				 params:    map[string]string{"ServerIP": serverIP},
				 target:    fmt.Sprintf("%s:8080", serverIP),
				 logChecks: []string{"dstip", "action: DENY"},
			 },
		 }
 
		 for _, sc := range scenarios {
			 sc := sc
			 ctx.NewSubTest(sc.name).Run(func(t framework.TestContext) {
				 // Apply the policy
				 istioComponent.ConfigIstio().
					 Eval(ns, sc.params, sc.template).
					 ApplyOrFail(t)
				 defer osExec.Command("kubectl", "delete", "authorizationpolicy", sc.name, "-n", ns, "--ignore-not-found").Run()
 
				 time.Sleep(3 * time.Second) // let policy propagate
 
				 // Fortio load should return Code -1
				 out, _ := osExec.Command("kubectl", "exec", "-n", ns, clientPod, "--",
					 "fortio", "load", "-c", "1", "-n", "1", "-qps", "0", sc.target).
					 CombinedOutput()
				 if !strings.Contains(string(out), "Code -1") {
					 t.Fatalf("expected denied (Code -1), got:\n%s", out)
				 }
 
				 // Verify XDP logs
				 kmeshPod := podName("kmesh-system", "")
				 logs, _ := osExec.Command("kubectl", "logs", "-n", "kmesh-system", kmeshPod).CombinedOutput()
				 for _, chk := range sc.logChecks {
					 if !strings.Contains(string(logs), chk) {
						 t.Fatalf("log did not contain %q", chk)
					 }
				 }
			 })
		 }
	 })
 }
 