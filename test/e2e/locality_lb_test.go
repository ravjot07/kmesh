//go:build integ
// +build integ

/*
Integration tests for Kmesh Locality Load Balancing (L4).

Exercises:
  1) PreferClose via spec.trafficDistribution
  2) PreferClose via annotation
  3) Local strict via spec.internalTrafficPolicy: Local
  4) Subzone distribution across two fallback pods

We label the Kind worker nodes with topology labels and pin
pods via nodeSelector. DNS races are avoided by fetching the
ClusterIP and using curl --resolve (with IPv6 brackets).
*/

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

/* ──────────────────────────── helpers ──────────────────────────── */

func run(ctx framework.TestContext, cmd string) string {
	out, err := shell.Execute(true, cmd)
	if err != nil {
		ctx.Fatalf("cmd %q failed: %v\n%s", cmd, err, out)
	}
	ctx.Logf(">>> %s\n%s", cmd, out)
	return out
}

func apply(ctx framework.TestContext, ns, yaml string) {
	dir := ctx.CreateTmpDirectoryOrFail("lb")
	f := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(f, []byte(yaml), 0644); err != nil {
		ctx.Fatalf("write %s: %v", f, err)
	}
	ctx.Logf(">>> Applying in %q:\n%s", ns, yaml)
	run(ctx, fmt.Sprintf("kubectl apply -n %s -f %s", ns, f))
}

func waitDep(ctx framework.TestContext, ns, name string) {
	run(ctx, fmt.Sprintf("kubectl wait --for=condition=available deployment/%s -n %s --timeout=120s", name, ns))
}

func clusterIP(ctx framework.TestContext, ns string) string {
	ip := run(ctx, fmt.Sprintf("kubectl get svc helloworld -n %s -o=jsonpath={.spec.clusterIP}", ns))
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	return ip
}

func sleepPod(ctx framework.TestContext, ns string) string {
	return run(ctx, fmt.Sprintf("kubectl get pod -n %s -l app=sleep -o=jsonpath={.items[0].metadata.name}", ns))
}

func curl(ctx framework.TestContext, ns, pod, fqdn, ip string) string {
	out, _ := shell.Execute(false,
		fmt.Sprintf("kubectl exec -n %s %s -- curl -sSL --resolve %s:5000:%s http://%s:5000/hello",
			ns, pod, fqdn, ip, fqdn))
	return out
}

func labelNodes(ctx framework.TestContext) {
	run(ctx, "kubectl label node kmesh-testing-worker topology.kubernetes.io/region=region "+
		"topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone1 --overwrite")
	run(ctx, "kubectl label node kmesh-testing-control-plane topology.kubernetes.io/region=region "+
		"topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone2 --overwrite")
}

/* ─────────────── YAML generators (pure block style) ────────────── */

func svcYAML(ns, extraMeta, extraSpec string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: helloworld
  namespace: %s
  labels:
    app: helloworld
%s
spec:
  selector:
    app: helloworld
  ports:
  - name: http
    port: 5000
    targetPort: 5000
%s
`, ns, extraMeta, extraSpec)
}

func deployYAML(ns, ver, node string) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels:
    app: helloworld
    version: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: %s
  template:
    metadata:
      labels:
        app: helloworld
        version: %s
    spec:
      nodeSelector:
        kubernetes.io/hostname: %s
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        env:
        - name: SERVICE_VERSION
          value: %s
        ports:
        - containerPort: 5000
`, ver, ns, ver, ver, ver, node, ver)
}

func sleepYAML(ns string) string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
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
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
`, ns)
}

/* ─────────────────────── Test 1 – PreferClose (spec) ────────────────────── */

func TestLocality_PreferClose_Spec(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		labelNodes(ctx)

		ns := "sample-pc-spec"
		fqdn := "helloworld." + ns + ".svc.cluster.local"
		run(ctx, "kubectl create namespace "+ns)

		apply(ctx, ns, svcYAML(ns, "", "  trafficDistribution: PreferClose"))
		apply(ctx, ns, deployYAML(ns, "sub1", "kmesh-testing-worker"))
		apply(ctx, ns, deployYAML(ns, "sub2", "kmesh-testing-control-plane"))
		apply(ctx, ns, sleepYAML(ns))

		for _, d := range []string{"helloworld-sub1", "helloworld-sub2", "sleep"} {
			waitDep(ctx, ns, d)
		}

		ip, pod := clusterIP(ctx, ns), sleepPod(ctx, ns)

		// should hit only sub1
		for i := 0; i < 10; i++ {
			if out := curl(ctx, ns, pod, fqdn, ip); strings.Contains(out, "sub2") {
				ctx.Fatalf("remote seen before fail‑over: %s", out)
			} else if strings.Contains(out, "sub1") {
				break
			}
			time.Sleep(time.Second)
		}

		run(ctx, "kubectl delete deployment helloworld-sub1 -n "+ns)
		retry.UntilSuccessOrFail(ctx, func() error {
			if strings.Contains(curl(ctx, ns, pod, fqdn, ip), "sub2") {
				return nil
			}
			return fmt.Errorf("not remote yet")
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second))
	})
}

/* ─────────────────── Test 2 – PreferClose (annotation) ──────────────────── */

func TestLocality_PreferClose_Annotation(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		labelNodes(ctx)
		ns := "sample-pc-annot"
		fqdn := "helloworld." + ns + ".svc.cluster.local"
		run(ctx, "kubectl create namespace "+ns)

		meta := "  annotations:\n    networking.istio.io/traffic-distribution: PreferClose\n"
		apply(ctx, ns, svcYAML(ns, meta, ""))
		apply(ctx, ns, deployYAML(ns, "sub1", "kmesh-testing-worker"))
		apply(ctx, ns, deployYAML(ns, "sub2", "kmesh-testing-control-plane"))
		apply(ctx, ns, sleepYAML(ns))

		for _, d := range []string{"helloworld-sub1", "helloworld-sub2", "sleep"} {
			waitDep(ctx, ns, d)
		}

		ip, pod := clusterIP(ctx, ns), sleepPod(ctx, ns)

		for i := 0; i < 10; i++ {
			if out := curl(ctx, ns, pod, fqdn, ip); strings.Contains(out, "sub2") {
				ctx.Fatalf("remote seen before fail‑over: %s", out)
			} else if strings.Contains(out, "sub1") {
				break
			}
			time.Sleep(time.Second)
		}

		run(ctx, "kubectl delete deployment helloworld-sub1 -n "+ns)
		retry.UntilSuccessOrFail(ctx, func() error {
			if strings.Contains(curl(ctx, ns, pod, fqdn, ip), "sub2") {
				return nil
			}
			return fmt.Errorf("not remote yet")
		}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second))
	})
}

/* ─────────────── Test 3 – internalTrafficPolicy: Local ─────────────── */

func TestLocality_LocalStrict(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		labelNodes(ctx)
		ns := "sample-local"
		fqdn := "helloworld." + ns + ".svc.cluster.local"
		run(ctx, "kubectl create namespace "+ns)

		apply(ctx, ns, svcYAML(ns, "", "  internalTrafficPolicy: Local"))
		apply(ctx, ns, deployYAML(ns, "sub1", "kmesh-testing-worker"))
		apply(ctx, ns, deployYAML(ns, "sub2", "kmesh-testing-control-plane"))
		apply(ctx, ns, sleepYAML(ns))

		for _, d := range []string{"helloworld-sub1", "helloworld-sub2", "sleep"} {
			waitDep(ctx, ns, d)
		}

		ip, pod := clusterIP(ctx, ns), sleepPod(ctx, ns)
		if out := curl(ctx, ns, pod, fqdn, ip); !strings.Contains(out, "sub1") {
			ctx.Fatalf("expected local sub1, got %s", out)
		}

		run(ctx, "kubectl delete deployment helloworld-sub1 -n "+ns)
		time.Sleep(5 * time.Second)
		if out := curl(ctx, ns, pod, fqdn, ip); out != "" {
			ctx.Fatalf("traffic should drop after local deletion, got %s", out)
		}
	})
}

/* ─────────────── Test 4 – distribution across two remotes ─────────────── */

func TestLocality_SubzoneDistribution(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		labelNodes(ctx)
		ns := "sample-dist"
		fqdn := "helloworld." + ns + ".svc.cluster.local"
		run(ctx, "kubectl create namespace "+ns)

		apply(ctx, ns, svcYAML(ns, "", "  trafficDistribution: PreferClose"))
		apply(ctx, ns, deployYAML(ns, "sub1", "kmesh-testing-worker"))
		apply(ctx, ns, deployYAML(ns, "sub2a", "kmesh-testing-control-plane"))
		apply(ctx, ns, deployYAML(ns, "sub2b", "kmesh-testing-control-plane"))
		apply(ctx, ns, sleepYAML(ns))

		for _, d := range []string{"helloworld-sub1", "helloworld-sub2a", "helloworld-sub2b", "sleep"} {
			waitDep(ctx, ns, d)
		}

		run(ctx, "kubectl delete deployment helloworld-sub1 -n "+ns)
		ip, pod := clusterIP(ctx, ns), sleepPod(ctx, ns)

		cnt := map[string]int{}
		for i := 0; i < 30; i++ {
			out := curl(ctx, ns, pod, fqdn, ip)
			for _, v := range []string{"sub2a", "sub2b"} {
				if strings.Contains(out, v) {
					cnt[v]++
				}
			}
			time.Sleep(200 * time.Millisecond)
		}

		if cnt["sub2a"] == 0 || cnt["sub2b"] == 0 {
			ctx.Fatalf("traffic not balanced: %+v", cnt)
		}
	})
}
