//go:build integ
// +build integ

// CODE Copied and modified from https://github.com/istio/istio
// more specifically: https://github.com/istio/istio/blob/master/pkg/test/framework/components/istio/ingress.go
//
// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kmesh

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/shell"
	"istio.io/istio/pkg/test/util/retry"
)

// applyManifest writes the provided manifest into a temp file and applies it via kubectl.
func applyManifest(ns, manifest string) error {
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
	_, err = shell.Execute(true, "kubectl apply -n "+ns+" -f "+tmp.Name())
	return err
}

// extractResolvedIP parses nslookup output and returns the first non-server Address.
func extractResolvedIP(nslookup string) string {
	var addrs []string
	for _, line := range strings.Split(nslookup, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Address:") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			ip := parts[1]
			// skip lines containing :53 (server)
			if strings.Contains(ip, ":53") {
				continue
			}
			addrs = append(addrs, ip)
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

func TestLocalityLoadBalancing_PreferClose(t *testing.T) {
	const ns = "sample"
	const service = "helloworld"
	const fqdn = service + "." + ns + ".svc.cluster.local"
	localVer := "region.zone1.subzone1"
	remoteVer := "region.zone1.subzone2"

	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		// 1) Create namespace (ignore if exists).
		if _, err := shell.Execute(true, "kubectl create namespace "+ns); err != nil {
			ctx.Logf("namespace %q may already exist: %v", ns, err)
		}

		// 2) Apply Service (PreferClose)
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: ` + service + `
  namespace: ` + ns + `
  labels:
    app: helloworld
spec:
  selector:
    app: helloworld
  ports:
  - port: 5000
    name: http
  trafficDistribution: PreferClose
`
		if err := applyManifest(ns, serviceYAML); err != nil {
			ctx.Fatalf("applyService failed: %v", err)
		}

		// 3) Deploy local instance
		depLocal := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-region-zone1-subzone1
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: region.zone1.subzone1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: region.zone1.subzone1
  template:
    metadata:
      labels:
        app: helloworld
        version: region.zone1.subzone1
    spec:
      containers:
      - name: helloworld
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone1
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		if err := applyManifest(ns, depLocal); err != nil {
			ctx.Fatalf("applyLocalDeployment failed: %v", err)
		}

		// 4) Deploy remote instance
		depRemote := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-region-zone1-subzone2
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: region.zone1.subzone2
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: region.zone1.subzone2
  template:
    metadata:
      labels:
        app: helloworld
        version: region.zone1.subzone2
    spec:
      containers:
      - name: helloworld
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone2
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-control-plane
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
        effect: "NoSchedule"
`
		if err := applyManifest(ns, depRemote); err != nil {
			ctx.Fatalf("applyRemoteDeployment failed: %v", err)
		}

		// 5) Deploy sleep client
		clientDep := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: ` + ns + `
  labels:
    app: sleep
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sleep
  template:
    metadata:
      labels:
        app: sleep
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
        imagePullPolicy: IfNotPresent
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		if err := applyManifest(ns, clientDep); err != nil {
			ctx.Fatalf("applyClientDeployment failed: %v", err)
		}

		// 6) Wait for deployments to be available
		for _, d := range []string{
			"helloworld-region-zone1-subzone1",
			"helloworld-region-zone1-subzone2",
			"sleep",
		} {
			cmd := fmt.Sprintf("kubectl wait --for=condition=available deployment/%s -n %s --timeout=120s", d, ns)
			if _, err := shell.Execute(true, cmd); err != nil {
				ctx.Fatalf("waiting for %s failed: %v", d, err)
			}
		}

		// 7) Identify sleep pod
		sleepPod, err := shell.Execute(true, "kubectl get pod -n "+ns+" -l app=sleep -o jsonpath='{.items[0].metadata.name}'")
		if err != nil || sleepPod == "" {
			ctx.Fatalf("failed to get sleep pod: %v", err)
		}

		// 8) Resolve Service IP via nslookup
		nslookup, _ := shell.Execute(true,
			fmt.Sprintf("kubectl exec -n %s %s -- nslookup %s", ns, sleepPod, fqdn))
		ctx.Logf("nslookup output:\n%s", nslookup)
		resolvedIP := extractResolvedIP(nslookup)
		if resolvedIP == "" {
			ctx.Fatalf("failed to extract IP from nslookup")
		}
		ctx.Logf("resolved %s to %s", fqdn, resolvedIP)

		// 9) Test PreferClose: expect local
		ctx.Log("Testing PreferClose: local first")
		if err := retry.Until(func() bool {
			out, err := shell.Execute(false,
				"kubectl exec -n "+ns+" "+sleepPod+
					" -- curl -sSL -v --resolve "+fqdn+":5000:"+resolvedIP+
					" http://"+fqdn+":5000/hello")
			ctx.Logf("curl output: %q, err: %v", out, err)
			return err == nil && strings.Contains(out, localVer)
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second)); err != nil {
			ctx.Fatalf("PreferClose local check failed: %v", err)
		}

		// 10) Test failover: delete local, expect remote
		ctx.Log("Deleting local deployment to test failover")
		if _, err := shell.Execute(true,
			"kubectl delete deployment helloworld-region-zone1-subzone1 -n "+ns); err != nil {
			ctx.Fatalf("deleting local deployment failed: %v", err)
		}
		ctx.Log("Testing PreferClose: remote after failover")
		if err := retry.Until(func() bool {
			out, err := shell.Execute(false,
				"kubectl exec -n "+ns+" "+sleepPod+
					" -- curl -sSL -v --resolve "+fqdn+":5000:"+resolvedIP+
					" http://"+fqdn+":5000/hello")
			ctx.Logf("curl after delete: %q, err: %v", out, err)
			return err == nil && strings.Contains(out, remoteVer)
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second)); err != nil {
			ctx.Fatalf("PreferClose failover check failed: %v", err)
		}
	})
}

func TestLocalityLoadBalancing_Local(t *testing.T) {
	const ns = "sample"
	const service = "helloworld"
	const fqdn = service + "." + ns + ".svc.cluster.local"
	localVer := "region.zone1.subzone1"
	remoteVer := "region.zone1.subzone2"

	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		// 1) Create namespace if needed
		if _, err := shell.Execute(true, "kubectl create namespace "+ns); err != nil {
			ctx.Logf("namespace %q may already exist: %v", ns, err)
		}

		// 2) Apply Service in Local mode
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: ` + service + `
  namespace: ` + ns + `
  labels:
    app: helloworld
spec:
  selector:
    app: helloworld
  ports:
  - port: 5000
    name: http
  trafficDistribution: Local
`
		if err := applyManifest(ns, serviceYAML); err != nil {
			ctx.Fatalf("applyService failed: %v", err)
		}

		// 3) Apply same depLocal, depRemote, clientDep as above
		if err := applyManifest(ns, depLocalYAML(ns)); err != nil {
			ctx.Fatalf("applyManifest(depLocal) failed: %v", err)
		}
		if err := applyManifest(ns, depRemoteYAML(ns)); err != nil {
			ctx.Fatalf("applyManifest(depRemote) failed: %v", err)
		}
		if err := applyManifest(ns, clientDepYAML(ns)); err != nil {
			ctx.Fatalf("applyManifest(clientDep) failed: %v", err)
		}

		// 4) Wait for deployments
		for _, d := range []string{localVer, remoteVer, "sleep"} {
			name := fmt.Sprintf("helloworld-%s", strings.ReplaceAll(d, ".", "-"))
			cmd := fmt.Sprintf("kubectl wait --for=condition=available deployment/%s -n %s --timeout=120s", name, ns)
			if _, err := shell.Execute(true, cmd); err != nil {
				ctx.Fatalf("waiting for %s failed: %v", name, err)
			}
		}

		// 5) Identify sleep pod
		sleepPod, err := shell.Execute(true, "kubectl get pod -n "+ns+" -l app=sleep -o jsonpath='{.items[0].metadata.name}'")
		if err != nil || sleepPod == "" {
			ctx.Fatalf("failed to get sleep pod: %v", err)
		}

		// 6) Resolve IP
		nslookup, _ := shell.Execute(true, "kubectl exec -n "+ns+" "+sleepPod+" -- nslookup "+fqdn)
		ip := extractResolvedIP(nslookup)
		if ip == "" {
			ctx.Fatalf("failed to resolve %s", fqdn)
		}

		// 7) Initial request: must hit local
		out, err := shell.Execute(false,
			"kubectl exec -n "+ns+" "+sleepPod+
				" -- curl -sSL -v --resolve "+fqdn+":5000:"+ip+
				" http://"+fqdn+":5000/hello")
		if err != nil || !strings.Contains(out, localVer) {
			ctx.Fatalf("Local mode initial expected %q, got %q err %v", localVer, out, err)
		}

		// 8) Delete local, request should fail (no fallback)
		shell.Execute(true, "kubectl delete deployment helloworld-region-zone1-subzone1 -n "+ns)
		time.Sleep(5 * time.Second)

		_, err = shell.Execute(false,
			"kubectl exec -n "+ns+" "+sleepPod+
				" -- curl -m 5 -sSL -v --resolve "+fqdn+":5000:"+ip+
				" http://"+fqdn+":5000/hello")
		if err == nil {
			ctx.Fatalf("Local mode should not fallback, but request succeeded")
		}
	})
}

// Helper YAML generators
func depLocalYAML(ns string) string {
	return `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-region-zone1-subzone1
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: region.zone1.subzone1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: region.zone1.subzone1
  template:
    metadata:
      labels:
        app: helloworld
        version: region.zone1.subzone1
    spec:
      containers:
      - name: helloworld
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone1
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
}

func depRemoteYAML(ns string) string {
	return `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-region-zone1-subzone2
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: region.zone1.subzone2
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: region.zone1.subzone2
  template:
    metadata:
      labels:
        app: helloworld
        version: region.zone1.subzone2
    spec:
      containers:
      - name: helloworld
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone2
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-control-plane
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
        effect: "NoSchedule"
`
}

func clientDepYAML(ns string) string {
	return `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: ` + ns + `
  labels:
    app: sleep
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sleep
  template:
    metadata:
      labels:
        app: sleep
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
        imagePullPolicy: IfNotPresent
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
}
