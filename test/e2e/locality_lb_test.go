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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/shell"
	"istio.io/istio/pkg/test/util/retry"
)

// runCommand runs a kubectl command via shell.Execute(true, ...) and fatals on error.
func runCommand(ctx framework.TestContext, cmd string) string {
	out, err := shell.Execute(true, cmd)
	if err != nil {
		ctx.Fatalf("Command %q failed: %v\n%s", cmd, err, out)
	}
	return out
}

// applyManifest writes `manifest` into a tmp file and kubectl-applies it.
func applyManifest(ctx framework.TestContext, ns, manifest string) {
	tmp := ctx.CreateTmpDirectoryOrFail("kmesh-lb")
	path := filepath.Join(tmp, "m.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		ctx.Fatalf("WriteFile(%s) failed: %v", path, err)
	}
	runCommand(ctx, fmt.Sprintf("kubectl apply -n %s -f %s", ns, path))
}

// getClusterIP returns the ClusterIP of service `svc` in `ns`.
func getClusterIP(ctx framework.TestContext, ns, svc string) string {
	out := runCommand(ctx, fmt.Sprintf(
		"kubectl get svc %s -n %s -o jsonpath={.spec.clusterIP}", svc, ns))
	if out == "" {
		ctx.Fatalf("Empty ClusterIP for %s/%s", ns, svc)
	}
	ctx.Logf("ClusterIP for %s/%s = %s", ns, svc, out)
	return out
}

// getSleepPod returns the name of the sleep pod in `ns`.
func getSleepPod(ctx framework.TestContext, ns string) string {
	out := runCommand(ctx, fmt.Sprintf(
		"kubectl get pod -n %s -l app=sleep -o jsonpath={.items[0].metadata.name}", ns))
	if out == "" {
		ctx.Fatalf("No sleep pod found in %s", ns)
	}
	ctx.Logf("sleep pod = %s", out)
	return out
}

// curlHello execs into sleep pod and curls fqdn:5000 via --resolve ip.
func curlHello(ctx framework.TestContext, ns, pod, fqdn, ip string) (string, error) {
	cmd := fmt.Sprintf(
		"kubectl exec -n %s %s -- curl -sSL -v --resolve %s:5000:%s http://%s:5000/hello",
		ns, pod, fqdn, ip, fqdn)
	return shell.Execute(false, cmd)
}

// waitForDeployment ensures the named deployment in `ns` is Available.
func waitForDeployment(ctx framework.TestContext, ns, name string) {
	cmd := fmt.Sprintf(
		"kubectl wait --for=condition=available deployment/%s -n %s --timeout=120s",
		name, ns)
	runCommand(ctx, cmd)
}

// Test 1: PreferClose via spec.trafficDistribution
func TestLocality_PreferClose_Spec(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ns, svc := "sample-pc-spec", "helloworld"
		fqdn := svc + "." + ns + ".svc.cluster.local"
		localVer, remoteVer := "sub1", "sub2"

		runCommand(ctx, "kubectl create namespace "+ns)

		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels: {app: helloworld}
spec:
  selector: {app: helloworld}
  ports: [{port: 5000, name: http}]
  trafficDistribution: PreferClose
`, svc, ns))

		// Local pod (sub1)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

		// Remote pod (sub2)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-control-plane}
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: Exists
        effect: NoSchedule
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))

		// Sleep client
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
  labels: {app: sleep}
spec:
  replicas: 1
  selector: {matchLabels: {app: sleep}}
  template:
    metadata: {labels: {app: sleep}}
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, ns))

		waitForDeployment(ctx, ns, "helloworld-"+localVer)
		waitForDeployment(ctx, ns, "helloworld-"+remoteVer)
		waitForDeployment(ctx, ns, "sleep")

		ip := getClusterIP(ctx, ns, svc)
		pod := getSleepPod(ctx, ns)

		ctx.Log("PreferClose(spec): expect only local")
		sawLocal := false
		for i := 0; i < 10; i++ {
			out, _ := curlHello(ctx, ns, pod, fqdn, ip)
			ctx.Logf("curl #%d: %s", i+1, out)
			if strings.Contains(out, remoteVer) {
				ctx.Fatalf("got remote before removal: %q", out)
			}
			if strings.Contains(out, localVer) {
				sawLocal = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !sawLocal {
			ctx.Fatalf("never saw local response")
		}

		ctx.Log("Deleting local to force failover")
		runCommand(ctx, "kubectl delete deployment helloworld-"+localVer+" -n "+ns)
		retry.UntilSuccessOrFail(ctx, func() error {
			out, _ := curlHello(ctx, ns, pod, fqdn, ip)
			if !strings.Contains(out, remoteVer) {
				return fmt.Errorf("still not remote: %q", out)
			}
			return nil
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second))
	})
}

// Test 2: PreferClose via annotation
func TestLocality_PreferClose_Annotation(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ns, svc := "sample-pc-annot", "helloworld"
		fqdn := svc + "." + ns + ".svc.cluster.local"
		localVer, remoteVer := "sub1", "sub2"

		runCommand(ctx, "kubectl create namespace "+ns)

		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  annotations:
    networking.istio.io/traffic-distribution: PreferClose
  labels: {app: helloworld}
spec:
  selector: {app: helloworld}
  ports: [{port: 5000, name: http}]
`, svc, ns))

		// Local pod (sub1)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

		// Remote pod (sub2)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-control-plane}
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: Exists
        effect: NoSchedule
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))

		// Sleep client
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
  labels: {app: sleep}
spec:
  replicas: 1
  selector: {matchLabels: {app: sleep}}
  template:
    metadata: {labels: {app: sleep}}
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, ns))

		waitForDeployment(ctx, ns, "helloworld-"+localVer)
		waitForDeployment(ctx, ns, "helloworld-"+remoteVer)
		waitForDeployment(ctx, ns, "sleep")

		ip := getClusterIP(ctx, ns, svc)
		pod := getSleepPod(ctx, ns)

		ctx.Log("PreferClose(annot): expect only local")
		sawLocal := false
		for i := 0; i < 10; i++ {
			out, _ := curlHello(ctx, ns, pod, fqdn, ip)
			ctx.Logf("curl #%d: %s", i+1, out)
			if strings.Contains(out, remoteVer) {
				ctx.Fatalf("got remote prematurely: %q", out)
			}
			if strings.Contains(out, localVer) {
				sawLocal = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !sawLocal {
			ctx.Fatalf("never saw local")
		}

		ctx.Log("Deleting local for annotation test failover")
		runCommand(ctx, "kubectl delete deployment helloworld-"+localVer+" -n "+ns)
		retry.UntilSuccessOrFail(ctx, func() error {
			out, _ := curlHello(ctx, ns, pod, fqdn, ip)
			if !strings.Contains(out, remoteVer) {
				return fmt.Errorf("still not remote: %q", out)
			}
			return nil
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second))
	})
}

// Test 3: Local strict via internalTrafficPolicy
func TestLocality_LocalStrict(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ns, svc := "sample-local", "helloworld"
		fqdn := svc + "." + ns + ".svc.cluster.local"
		localVer, remoteVer := "sub1", "sub2"

		runCommand(ctx, "kubectl create namespace "+ns)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels: {app: helloworld}
spec:
  selector: {app: helloworld}
  ports: [{port: 5000, name: http}]
  internalTrafficPolicy: Local
`, svc, ns))

		// Local pod
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

		// Remote pod
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-control-plane}
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: Exists
        effect: NoSchedule
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))

		// Sleep client
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
  labels: {app: sleep}
spec:
  replicas: 1
  selector: {matchLabels: {app: sleep}}
  template:
    metadata: {labels: {app: sleep}}
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, ns))

		waitForDeployment(ctx, ns, "helloworld-"+localVer)
		waitForDeployment(ctx, ns, "helloworld-"+remoteVer)
		waitForDeployment(ctx, ns, "sleep")

		out, _ := curlHello(ctx, ns, getSleepPod(ctx, ns), fqdn, getClusterIP(ctx, ns, svc))
		if !strings.Contains(out, localVer) {
			ctx.Fatalf("Local strict initial: expected %q, got %q", localVer, out)
		}

		runCommand(ctx, "kubectl delete deployment helloworld-"+localVer+" -n "+ns)
		time.Sleep(5 * time.Second)
		if out, err := curlHello(ctx, ns, getSleepPod(ctx, ns), fqdn, getClusterIP(ctx, ns, svc)); err == nil {
			ctx.Fatalf("Local strict fallback should fail, but got %q", out)
		}
	})
}

// Test 4: Subzone distribution across two fallback pods
func TestLocality_SubzoneDistribution(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ns, svc := "sample-dist", "helloworld"
		fqdn := svc + "." + ns + ".svc.cluster.local"
		localVer := "sub1"
		rem1, rem2 := "sub2-A", "sub2-B"

		runCommand(ctx, "kubectl create namespace "+ns)
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels: {app: helloworld}
spec:
  selector: {app: helloworld}
  ports: [{port: 5000, name: http}]
  trafficDistribution: PreferClose
`, svc, ns))

		// Local pod
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

		// Remote pod A
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-control-plane}
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: Exists
        effect: NoSchedule
`, rem1, ns, rem1, rem1, rem1, rem1))

		// Remote pod B
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld, version: %s}
spec:
  replicas: 1
  selector: {matchLabels: {app: helloworld, version: %s}}
  template:
    metadata: {labels: {app: helloworld, version: %s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env: [{name: SERVICE_VERSION, value: %s}]
        ports: [{containerPort: 5000}]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-control-plane}
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: Exists
        effect: NoSchedule
`, rem2, ns, rem2, rem2, rem2, rem2))

		// Sleep client
		applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
  labels: {app: sleep}
spec:
  replicas: 1
  selector: {matchLabels: {app: sleep}}
  template:
    metadata: {labels: {app: sleep}}
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
      nodeSelector: {kubernetes.io/hostname: kmesh-testing-worker}
`, ns))

		waitForDeployment(ctx, ns, "helloworld-"+localVer)
		waitForDeployment(ctx, ns, "helloworld-"+rem1)
		waitForDeployment(ctx, ns, "helloworld-"+rem2)
		waitForDeployment(ctx, ns, "sleep")

		runCommand(ctx, "kubectl delete deployment helloworld-"+localVer+" -n "+ns)
		ip := getClusterIP(ctx, ns, svc)
		pod := getSleepPod(ctx, ns)

		counts := map[string]int{}
		for i := 0; i < 20; i++ {
			out, _ := curlHello(ctx, ns, pod, fqdn, ip)
			for _, v := range []string{rem1, rem2} {
				if strings.Contains(out, v) {
					counts[v]++
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		ctx.Logf("Distribution: %+v", counts)
		if counts[rem1] == 0 || counts[rem2] == 0 {
			ctx.Fatalf("Expected both rem1 and rem2 to serve at least once, got %+v", counts)
		}
	})
}
