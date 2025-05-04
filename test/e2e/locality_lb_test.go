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

// runCommand shells out and fatals on error. Logs both command and output.
func runCommand(ctx framework.TestContext, cmd string) string {
    out, err := shell.Execute(true, cmd)
    if err != nil {
        ctx.Fatalf("Command %q failed: %v\n%s", cmd, err, out)
    }
    ctx.Logf(">>> Command succeeded: %s\n%s", cmd, out)
    return out
}

// applyManifest writes the manifest to a temp file and kubectl-applies it with debug logs.
func applyManifest(ctx framework.TestContext, ns, mani string) {
    ctx.Logf(">>> Applying to namespace %q manifest:\n%s", ns, mani)
    dir := ctx.CreateTmpDirectoryOrFail("kmesh-lb")
    path := filepath.Join(dir, "m.yaml")
    if err := os.WriteFile(path, []byte(mani), 0644); err != nil {
        ctx.Fatalf("WriteFile(%s) failed: %v", path, err)
    }
    content, _ := os.ReadFile(path)
    ctx.Logf(">>> On-disk manifest at %s:\n%s", path, string(content))
    runCommand(ctx, fmt.Sprintf("kubectl apply -n %s -f %s", ns, path))
}

// getClusterIP fetches and returns the ClusterIP for svc in ns, bracketed for IPv6.
func getClusterIP(ctx framework.TestContext, ns, svc string) string {
    ip := runCommand(ctx, fmt.Sprintf("kubectl get svc %s -n %s -o jsonpath={.spec.clusterIP}", svc, ns))
    if ip == "" {
        ctx.Fatalf("Empty ClusterIP for %s/%s", ns, svc)
    }
    if strings.Contains(ip, ":") {
        ip = "[" + ip + "]"
    }
    ctx.Logf("ClusterIP for %s/%s = %s", ns, svc, ip)
    return ip
}

// getSleepPod returns the name of the sleep pod in ns.
func getSleepPod(ctx framework.TestContext, ns string) string {
    pod := runCommand(ctx, fmt.Sprintf(
        "kubectl get pod -n %s -l app=sleep -o jsonpath={.items[0].metadata.name}", ns))
    if pod == "" {
        ctx.Fatalf("No sleep pod found in %s", ns)
    }
    ctx.Logf("sleep pod = %s", pod)
    return pod
}

// waitForDeployment waits until deployment/name in ns is available.
func waitForDeployment(ctx framework.TestContext, ns, name string) {
    runCommand(ctx, fmt.Sprintf(
        "kubectl wait --for=condition=available deployment/%s -n %s --timeout=120s",
        name, ns))
}

// curlHello execs into the sleep pod and curls the service FQDN via resolve.
func curlHello(ctx framework.TestContext, ns, pod, fqdn, ip string) (string, error) {
    cmd := fmt.Sprintf(
        "kubectl exec -n %s %s -- curl -sSL -v --resolve %s:5000:%s http://%s:5000/hello",
        ns, pod, fqdn, ip, fqdn)
    return shell.Execute(false, cmd)
}

// ---------------------------------------------------------------------------
// Test 1: PreferClose via spec.trafficDistribution
// ---------------------------------------------------------------------------
func TestLocality_PreferClose_Spec(t *testing.T) {
    framework.NewTest(t).Run(func(ctx framework.TestContext) {
        ctx.Log("=== START TestLocality_PreferClose_Spec ===")
        ctx.Log("Node labels:")
        runCommand(ctx, "kubectl get nodes --show-labels")
        ctx.Log("Existing Services across all namespaces:")
        runCommand(ctx, "kubectl get svc -A")

        // Label local and remote nodes
        runCommand(ctx, "kubectl label node kmesh-testing-worker topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone1 --overwrite")
        runCommand(ctx, "kubectl label node kmesh-testing-control-plane topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone2 --overwrite")

        ns, svc := "sample-pc-spec", "helloworld"
        fqdn := svc + "." + ns + ".svc.cluster.local"
        localVer, remoteVer := "sub1", "sub2"

        runCommand(ctx, "kubectl create namespace "+ns)

        // Service with spec trafficDistribution
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels: {app: helloworld}
spec:
  selector: {app: helloworld}
  ports: [{port:5000,name:http}]
  trafficDistribution: PreferClose
`, svc, ns))

        // Local Deployment on worker
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld,version:%s}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        env:[{name:SERVICE_VERSION,value:%s}]
        ports:[{containerPort:5000}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

        // Remote Deployment on control-plane
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: helloworld-%s
  namespace: %s
  labels: {app: helloworld,version:%s}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        env:[{name:SERVICE_VERSION,value:%s}]
        ports:[{containerPort:5000}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-control-plane}
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))

        // Sleep client on worker
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
  namespace: %s
  labels: {app: sleep}
spec:
  replicas:1
  selector:{matchLabels:{app:sleep}}
  template:
    metadata:{labels:{app:sleep}}
    spec:
      containers:
      - name: sleep
        image: curlimages/curl
        command:["/bin/sleep","infinity"]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, ns))

        waitForDeployment(ctx, ns, "helloworld-"+localVer)
        waitForDeployment(ctx, ns, "helloworld-"+remoteVer)
        waitForDeployment(ctx, ns, "sleep")

        ip := getClusterIP(ctx, ns, svc)
        pod := getSleepPod(ctx, ns)

        ctx.Log("PreferClose(spec): expecting only local")
        sawLocal := false
        for i := 0; i < 10; i++ {
            out, _ := curlHello(ctx, ns, pod, fqdn, ip)
            ctx.Logf("curl #%d: %s", i+1, out)
            if strings.Contains(out, remoteVer) {
                ctx.Fatalf("remote seen before deletion: %q", out)
            }
            if strings.Contains(out, localVer) {
                sawLocal = true
                break
            }
            time.Sleep(2 * time.Second)
        }
        if !sawLocal {
            ctx.Fatalf("never saw local (%s)", localVer)
        }

        ctx.Log("Deleting local to trigger failover")
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

// ---------------------------------------------------------------------------
// Test 2: PreferClose via annotation
// ---------------------------------------------------------------------------
func TestLocality_PreferClose_Annotation(t *testing.T) {
    framework.NewTest(t).Run(func(ctx framework.TestContext) {
        ctx.Log("=== START TestLocality_PreferClose_Annotation ===")
        ctx.Log("Node labels:")
        runCommand(ctx, "kubectl get nodes --show-labels")
        ctx.Log("Existing Services:")
        runCommand(ctx, "kubectl get svc -A")

        runCommand(ctx, "kubectl label node kmesh-testing-worker topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone1 --overwrite")
        runCommand(ctx, "kubectl label node kmesh-testing-control-plane topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone2 --overwrite")

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
  ports: [{port:5000,name:http}]
`, svc, ns))

        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-control-plane}
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:sleep,namespace:%s,labels:{app:sleep}}
spec:
  replicas:1
  selector:{matchLabels:{app:sleep}}
  template:
    metadata:{labels:{app:sleep}}
    spec:
      containers:[{name:sleep,image:curlimages/curl,command:["/bin/sleep","infinity"]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, ns))

        waitForDeployment(ctx, ns, "helloworld-"+localVer)
        waitForDeployment(ctx, ns, "helloworld-"+remoteVer)
        waitForDeployment(ctx, ns, "sleep")

        ip := getClusterIP(ctx, ns, svc)
        pod := getSleepPod(ctx, ns)

        ctx.Log("PreferClose(annot): expecting only local")
        sawLocal := false
        for i := 0; i < 10; i++ {
            out, _ := curlHello(ctx, ns, pod, fqdn, ip)
            ctx.Logf("curl #%d: %s", i+1, out)
            if strings.Contains(out, remoteVer) {
                ctx.Fatalf("remote seen too early: %q", out)
            }
            if strings.Contains(out, localVer) {
                sawLocal = true
                break
            }
            time.Sleep(2 * time.Second)
        }
        if !sawLocal {
            ctx.Fatalf("never saw local (%s)", localVer)
        }

        ctx.Log("Deleting local → failover")
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

// ---------------------------------------------------------------------------
// Test 3: Local strict via internalTrafficPolicy: Local
// ---------------------------------------------------------------------------
func TestLocality_LocalStrict(t *testing.T) {
    framework.NewTest(t).Run(func(ctx framework.TestContext) {
        ctx.Log("=== START TestLocality_LocalStrict ===")
        ctx.Log("Node labels:")
        runCommand(ctx, "kubectl get nodes --show-labels")
        ctx.Log("Services:")
        runCommand(ctx, "kubectl get svc -A")

        runCommand(ctx, "kubectl label node kmesh-testing-worker topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone1 --overwrite")
        runCommand(ctx, "kubectl label node kmesh-testing-control-plane topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone2 --overwrite")

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
  ports: [{port:5000,name:http}]
  internalTrafficPolicy: Local
`, svc, ns))

        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-control-plane}
`, remoteVer, ns, remoteVer, remoteVer, remoteVer, remoteVer))
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:sleep,namespace:%s,labels:{app:sleep}}
spec:
  replicas:1
  selector:{matchLabels:{app:sleep}}
  template:
    metadata:{labels:{app:sleep}}
    spec:
      containers:[{name:sleep,image:curlimages/curl,command:["/bin/sleep","infinity"]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
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

// ---------------------------------------------------------------------------
// Test 4: Subzone distribution across two fallback pods
// ---------------------------------------------------------------------------
func TestLocality_SubzoneDistribution(t *testing.T) {
    framework.NewTest(t).Run(func(ctx framework.TestContext) {
        ctx.Log("=== START TestLocality_SubzoneDistribution ===")
        ctx.Log("Node labels:")
        runCommand(ctx, "kubectl get nodes --show-labels")
        ctx.Log("Services:")
        runCommand(ctx, "kubectl get svc -A")

        runCommand(ctx, "kubectl label node kmesh-testing-worker topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone1 --overwrite")
        runCommand(ctx, "kubectl label node kmesh-testing-control-plane topology.kubernetes.io/region=region topology.kubernetes.io/zone=zone1 topology.kubernetes.io/subzone=subzone2 --overwrite")

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
  ports: [{port:5000,name:http}]
  trafficDistribution: PreferClose
`, svc, ns))

        // Local pod
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, localVer, ns, localVer, localVer, localVer, localVer))

        // Two remote pods on control-plane
        for _, v := range []string{rem1, rem2} {
            applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:helloworld-%s,namespace:%s,labels:{app:helloworld,version:%s}}
spec:
  replicas:1
  selector:{matchLabels:{app:helloworld,version:%s}}
  template:
    metadata:{labels:{app:helloworld,version:%s}}
    spec:
      containers:[{name:helloworld,image:docker.io/istio/examples-helloworld-v1,env:[{name:SERVICE_VERSION,value:%s}],ports:[{containerPort:5000}]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-control-plane}
`, v, ns, v, v, v, v))
        }

        // Sleep client
        applyManifest(ctx, ns, fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name:sleep,namespace:%s,labels:{app:sleep}}
spec:
  replicas:1
  selector:{matchLabels:{app:sleep}}
  template:
    metadata:{labels:{app:sleep}}
    spec:
      containers:[{name:sleep,image:curlimages/curl,command:["/bin/sleep","infinity"]}]
      nodeSelector:{kubernetes.io/hostname:kmesh-testing-worker}
`, ns))

        waitForDeployment(ctx, ns, "helloworld-"+localVer)
        waitForDeployment(ctx, ns, "helloworld-"+rem1)
        waitForDeployment(ctx, ns, "helloworld-"+rem2)
        waitForDeployment(ctx, ns, "sleep")

        // Remove local, test subzone distribution
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
            ctx.Fatalf("Expected both %s and %s, got %+v", rem1, rem2, counts)
        }
    })
}