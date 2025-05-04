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
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/shell"
	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const ns = "kmesh-system"

var k8sClient *kubernetes.Clientset

// getK8sClient attempts to load in-cluster config, then falls back to KUBECONFIG.
// Debug printouts trace each step.
func getK8sClient() (*kubernetes.Clientset, error) {
	if k8sClient != nil {
		fmt.Println("[DEBUG] Reusing existing k8sClient")
		return k8sClient, nil
	}

	// 1) Try in-cluster
	fmt.Println("[DEBUG] getK8sClient: attempting in-cluster config")
	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Printf("[DEBUG] in-cluster config failed: %v\n", err)

		// 2) Fallback to KUBECONFIG env var or default
		kubeconfig := os.Getenv("KUBECONFIG")
		fmt.Printf("[DEBUG] falling back to KUBECONFIG path: %q\n", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			fmt.Printf("[DEBUG] BuildConfigFromFlags failed: %v\n", err)
			return nil, fmt.Errorf("failed to load kubeconfig: %v", err)
		}
		fmt.Println("[DEBUG] successfully loaded config from KUBECONFIG")
	} else {
		fmt.Println("[DEBUG] successfully loaded in-cluster config")
	}

	// Create clientset
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("[DEBUG] NewForConfig failed: %v\n", err)
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}
	fmt.Println("[DEBUG] Kubernetes client created")
	k8sClient = client
	return k8sClient, nil
}

// applyManifest creates or updates a Service or Deployment from the given YAML manifest.
// It prints a timestamped debug log of the manifest.
func applyManifest(namespace, manifest string) error {
	ts := time.Now().Format(time.RFC3339)
	fmt.Printf("[DEBUG %s] applyManifest(namespace=%s):\n%s\n", ts, namespace, manifest)

	client, err := getK8sClient()
	if err != nil {
		return err
	}

	m := strings.TrimSpace(manifest)
	if m == "" {
		return nil
	}

	// Determine kind by string pattern
	switch {
	case strings.Contains(m, "kind: Service"):
		obj := &corev1.Service{}
		if err := yaml.Unmarshal([]byte(m), obj); err != nil {
			return err
		}
		obj.Namespace = namespace
		_, err = client.CoreV1().Services(namespace).Create(context.TODO(), obj, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = client.CoreV1().Services(namespace).Update(context.TODO(), obj, metav1.UpdateOptions{})
		}
		return err

	case strings.Contains(m, "kind: Deployment"):
		obj := &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(m), obj); err != nil {
			return err
		}
		obj.Namespace = namespace
		_, err = client.AppsV1().Deployments(namespace).Create(context.TODO(), obj, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = client.AppsV1().Deployments(namespace).Update(context.TODO(), obj, metav1.UpdateOptions{})
		}
		return err

	default:
		return fmt.Errorf("unsupported manifest kind")
	}
}

func TestLocalityLoadBalancing_PreferClose(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		// Service with PreferClose failover mode
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: helloworld
  namespace: ` + ns + `
  labels:
    app: helloworld
spec:
  selector:
    app: helloworld
  ports:
  - name: http
    port: 5000
    targetPort: 5000
  clusterIP: None
  trafficDistribution: PreferClose
`
		// Local instance on worker node
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
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone1
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		// Remote instance on control-plane with toleration
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
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone2
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-control-plane
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
        effect: "NoSchedule"
`
		// Sleep client to generate traffic
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
      terminationGracePeriodSeconds: 0
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
        imagePullPolicy: IfNotPresent
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`

		// Step 1: Apply manifests
		t.Logf("[STEP] Applying Service")
		if err := applyManifest(ns, serviceYAML); err != nil {
			t.Fatalf("applyManifest(Service) failed: %v", err)
		}
		t.Logf("[STEP] Applying Local Deployment")
		if err := applyManifest(ns, depLocal); err != nil {
			t.Fatalf("applyManifest(Local) failed: %v", err)
		}
		t.Logf("[STEP] Applying Remote Deployment")
		if err := applyManifest(ns, depRemote); err != nil {
			t.Fatalf("applyManifest(Remote) failed: %v", err)
		}
		t.Logf("[STEP] Applying Sleep client")
		if err := applyManifest(ns, clientDep); err != nil {
			t.Fatalf("applyManifest(Client) failed: %v", err)
		}

		// Step 2: Wait for readiness
		t.Logf("[STEP] Waiting for deployments availability")
		if out, err := shell.Execute(true, "kubectl wait --for=condition=available deployment -n "+ns+" --timeout=120s --all"); err != nil {
			t.Fatalf("waiting deployments failed: %v\n%s", err, out)
		} else {
			t.Logf("[DEBUG] wait output: %s", out)
		}

		// Step 3: Find sleep pod
		client, err := getK8sClient()
		if err != nil {
			t.Fatalf("getK8sClient failed: %v", err)
		}
		t.Logf("[STEP] Listing sleep pods")
		pods, err := client.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil {
			t.Fatalf("listing sleep pod failed: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Fatal("no sleep pod found")
		}
		sleepPod := pods.Items[0].Name
		t.Logf("[DEBUG] Sleep pod: %s", sleepPod)

		// Step 4: PreferClose - local preference
		t.Logf("[TEST] PreferClose mode: expect local instance")
		start := time.Now()
		for {
			out, err := shell.Execute(false, fmt.Sprintf(
				"kubectl exec -n %s %s -- curl -sS http://helloworld.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, ns))
			t.Logf("[DEBUG] curl output: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, "region.zone1.subzone1") {
				t.Logf("[PASS] received local response: %s", out)
				break
			}
			if time.Since(start) > 60*time.Second {
				t.Fatalf("PreferClose local timeout, last: %q, err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}

		// Step 5: Failover - delete local, expect remote
		t.Logf("[STEP] Deleting local deployment helloworld-region-zone1-subzone1")
		if err := client.AppsV1().Deployments(ns).Delete(context.TODO(), "helloworld-region-zone1-subzone1", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting local deployment failed: %v", err)
		}
		t.Logf("[TEST] PreferClose mode: expect remote instance after deletion")
		start = time.Now()
		for {
			out, err := shell.Execute(false, fmt.Sprintf(
				"kubectl exec -n %s %s -- curl -sS http://helloworld.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, ns))
			t.Logf("[DEBUG] curl after delete: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, "region.zone1.subzone2") {
				t.Logf("[PASS] received remote response: %s", out)
				break
			}
			if time.Since(start) > 60*time.Second {
				t.Fatalf("PreferClose failover timeout, last: %q, err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func TestLocalityLoadBalancing_Local(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		// Service with strict Local mode
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: helloworld
  namespace: ` + ns + `
  labels:
    app: helloworld
spec:
  selector:
    app: helloworld
  ports:
  - name: http
    port: 5000
    targetPort: 5000
  clusterIP: None
  trafficDistribution: Local
`
		// Local instance
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
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone1
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		// Remote instance
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
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: region.zone1.subzone2
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-control-plane
      tolerations:
      - key: "node-role.kubernetes.io/control-plane"
        operator: "Exists"
        effect: "NoSchedule"
`
		// Sleep client
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
      terminationGracePeriodSeconds: 0
      containers:
      - name: sleep
        image: curlimages/curl
        command: ["/bin/sleep","infinity"]
        imagePullPolicy: IfNotPresent
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`

		// Apply manifests
		t.Logf("[STEP] Applying Service (Local mode)")
		if err := applyManifest(ns, serviceYAML); err != nil {
			t.Fatalf("applyManifest(Service) failed: %v", err)
		}
		t.Logf("[STEP] Applying Local Deployment")
		if err := applyManifest(ns, depLocal); err != nil {
			t.Fatalf("applyManifest(Local) failed: %v", err)
		}
		t.Logf("[STEP] Applying Remote Deployment")
		if err := applyManifest(ns, depRemote); err != nil {
			t.Fatalf("applyManifest(Remote) failed: %v", err)
		}
		t.Logf("[STEP] Applying Sleep client")
		if err := applyManifest(ns, clientDep); err != nil {
			t.Fatalf("applyManifest(Client) failed: %v", err)
		}

		// Wait for readiness
		t.Logf("[STEP] Waiting for deployments availability")
		if out, err := shell.Execute(true, "kubectl wait --for=condition=available deployment -n "+ns+" --timeout=120s --all"); err != nil {
			t.Fatalf("waiting deployments failed: %v\n%s", err, out)
		} else {
			t.Logf("[DEBUG] wait output: %s", out)
		}

		// Discover sleep pod
		client, err := getK8sClient()
		if err != nil {
			t.Fatalf("getK8sClient failed: %v", err)
		}
		t.Logf("[STEP] Listing sleep pods")
		pods, err := client.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil {
			t.Fatalf("listing sleep pod failed: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Fatal("no sleep pod found")
		}
		sleepPod := pods.Items[0].Name
		t.Logf("[DEBUG] Sleep pod: %s", sleepPod)

		// Initial request: should hit local
		t.Logf("[TEST] Local mode initial request: expect local instance")
		out, err := shell.Execute(false, fmt.Sprintf(
			"kubectl exec -n %s %s -- curl -sS http://helloworld.%s.svc.cluster.local:5000/hello",
			ns, sleepPod, ns))
		t.Logf("[DEBUG] initial curl: %q, err: %v", out, err)
		if err != nil || !strings.Contains(out, "region.zone1.subzone1") {
			t.Fatalf("Local mode initial failed: %q, %v", out, err)
		}
		t.Log("[PASS] initial local endpoint OK")

		// Delete local: expect no fallback
		t.Logf("[STEP] Deleting local deployment helloworld-region-zone1-subzone1")
		if err := client.AppsV1().Deployments(ns).Delete(context.TODO(), "helloworld-region-zone1-subzone1", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting local deployment failed: %v", err)
		}
		time.Sleep(5 * time.Second)

		t.Logf("[TEST] Local mode after deletion: expect no remote fallback")
		out, err = shell.Execute(false, fmt.Sprintf(
			"kubectl exec -n %s %s -- curl -m 5 -sS http://helloworld.%s.svc.cluster.local:5000/hello",
			ns, sleepPod, ns))
		t.Logf("[DEBUG] post-delete curl: %q, err: %v", out, err)
		if err == nil && strings.Contains(out, "region.zone1.subzone2") {
			t.Fatalf("Local mode fallback occurred unexpectedly: %q", out)
		}
		t.Log("[PASS] strict Local mode verified: no remote fallback")
	})
}
