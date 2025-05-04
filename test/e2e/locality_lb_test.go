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
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/shell"
)

const (
	ns               = "sample"
	localVersion     = "region.zone1.subzone1"
	remoteVersion    = "region.zone1.subzone2"
	serviceName      = "helloworld"
	localDeployName  = "helloworld-region-zone1-subzone1"
	remoteDeployName = "helloworld-region-zone1-subzone2"
	sleepDeployName  = "sleep"
)

var clientset *kubernetes.Clientset

// getK8sClient initializes a Kubernetes clientset.
// It first tries in-cluster config; if that fails, it falls back to
// the KUBECONFIG environment variable, or ~/.kube/config.
func getK8sClient() (*kubernetes.Clientset, error) {
	if clientset != nil {
		return clientset, nil
	}
	// 1) Try in-cluster
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// 2) Fallback to kubeconfig
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home := os.Getenv("HOME")
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig (%s): %v", kubeconfig, err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}
	clientset = cs
	return clientset, nil
}

// applyManifest creates or updates a Service or Deployment from the given YAML.
func applyManifest(namespace, manifest string) error {
	cs, err := getK8sClient()
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(manifest)
	if trimmed == "" {
		return nil
	}
	// Service?
	if strings.Contains(trimmed, "kind: Service") {
		svc := &corev1.Service{}
		if err := yaml.Unmarshal([]byte(trimmed), svc); err != nil {
			return err
		}
		svc.Namespace = namespace
		_, err = cs.CoreV1().Services(namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = cs.CoreV1().Services(namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
		}
		return err
	}
	// Deployment?
	if strings.Contains(trimmed, "kind: Deployment") {
		dep := &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(trimmed), dep); err != nil {
			return err
		}
		dep.Namespace = namespace
		_, err = cs.AppsV1().Deployments(namespace).Create(context.TODO(), dep, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = cs.AppsV1().Deployments(namespace).Update(context.TODO(), dep, metav1.UpdateOptions{})
		}
		return err
	}
	return fmt.Errorf("unsupported manifest kind:\n%s", trimmed)
}

// ensureNamespace makes sure the test namespace exists.
func ensureNamespace(t *testing.T) {
	cs, err := getK8sClient()
	if err != nil {
		t.Fatalf("getK8sClient failed: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{}); err != nil {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if _, err := cs.CoreV1().Namespaces().Create(context.TODO(), n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create namespace %q: %v", ns, err)
		}
	}
}

func TestLocalityLoadBalancing_PreferClose(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		ensureNamespace(t)

		// 1) Service definition
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: ` + serviceName + `
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
		// 2) Local deployment (on worker)
		depLocal := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + localDeployName + `
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: ` + localVersion + `
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: ` + localVersion + `
  template:
    metadata:
      labels:
        app: helloworld
        version: ` + localVersion + `
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: ` + localVersion + `
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		// 3) Remote deployment (on control-plane)
		depRemote := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + remoteDeployName + `
  namespace: ` + ns + `
  labels:
    app: helloworld
    version: ` + remoteVersion + `
spec:
  replicas: 1
  selector:
    matchLabels:
      app: helloworld
      version: ` + remoteVersion + `
  template:
    metadata:
      labels:
        app: helloworld
        version: ` + remoteVersion + `
    spec:
      containers:
      - name: helloworld
        image: docker.io/istio/examples-helloworld-v1
        imagePullPolicy: IfNotPresent
        env:
        - name: SERVICE_VERSION
          value: ` + remoteVersion + `
        ports:
        - containerPort: 5000
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-control-plane
      tolerations:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
`
		// 4) Sleep client
		clientDep := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + sleepDeployName + `
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

		// Apply all manifests
		for name, manifest := range map[string]string{
			"service": serviceYAML,
			"local":   depLocal,
			"remote":  depRemote,
			"client":  clientDep,
		} {
			t.Logf("Applying %s manifest", name)
			if err := applyManifest(ns, manifest); err != nil {
				t.Fatalf("applyManifest(%s) failed: %v", name, err)
			}
		}

		// Wait for the helloworld pods and the sleep pod to be Ready
		waitCmd := fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=helloworld -n %s --timeout=120s", ns)
		if out, err := shell.Execute(true, waitCmd); err != nil {
			t.Fatalf("waiting for helloworld pods failed: %v\n%s", err, out)
		}
		waitCmd = fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=sleep -n %s --timeout=120s", ns)
		if out, err := shell.Execute(true, waitCmd); err != nil {
			t.Fatalf("waiting for sleep pod failed: %v\n%s", err, out)
		}

		// Identify the sleep pod
		cs, err := getK8sClient()
		if err != nil {
			t.Fatalf("getK8sClient failed: %v", err)
		}
		podList, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil || len(podList.Items) == 0 {
			t.Fatalf("failed to list sleep pod: %v", err)
		}
		sleepPod := podList.Items[0].Name

		// 1) Verify PreferClose: traffic to local pod first
		t.Log("Verifying PreferClose: expecting local version")
		start := time.Now()
		for {
			out, err := shell.Execute(false,
				fmt.Sprintf("kubectl exec -n %s %s -- curl -sS http://%s.%s.svc.cluster.local:5000/hello",
					ns, sleepPod, serviceName, ns))
			t.Logf("curl output: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, localVersion) {
				t.Logf("Got local response: %s", out)
				break
			}
			if time.Since(start) > 60*time.Second {
				t.Fatalf("PreferClose local check timed out, last: %q err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}

		// 2) Delete local pod and verify failover to remote
		t.Logf("Deleting local deployment %q", localDeployName)
		if err := cs.AppsV1().Deployments(ns).Delete(context.TODO(), localDeployName, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting local deployment failed: %v", err)
		}
		t.Log("Verifying PreferClose failover: expecting remote version")
		start = time.Now()
		for {
			out, err := shell.Execute(false,
				fmt.Sprintf("kubectl exec -n %s %s -- curl -sS http://%s.%s.svc.cluster.local:5000/hello",
					ns, sleepPod, serviceName, ns))
			t.Logf("curl output after delete: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, remoteVersion) {
				t.Logf("Got remote response: %s", out)
				break
			}
			if time.Since(start) > 60*time.Second {
				t.Fatalf("PreferClose failover check timed out, last: %q err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func TestLocalityLoadBalancing_Local(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		ensureNamespace(t)

		// Service in strict Local mode
		serviceYAML := `
apiVersion: v1
kind: Service
metadata:
  name: ` + serviceName + `
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
		// Reuse the same deployments and client as above
		depLocal := /* same as above */
		depRemote := /* same as above */
		clientDep := /* same as above */

		// Apply manifests
		for _, manifest := range []string{serviceYAML, depLocal, depRemote, clientDep} {
			if err := applyManifest(ns, manifest); err != nil {
				t.Fatalf("applyManifest failed: %v", err)
			}
		}

		// Wait for readiness
		shell.Execute(true, fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=helloworld -n %s --timeout=120s", ns))
		shell.Execute(true, fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=sleep -n %s --timeout=120s", ns))

		// Find the sleep pod
		cs, _ := getK8sClient()
		pods, _ := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		sleepPod := pods.Items[0].Name

		// 1) Initial request – must hit local
		out, err := shell.Execute(false,
			fmt.Sprintf("kubectl exec -n %s %s -- curl -sS http://%s.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, serviceName, ns))
		if err != nil || !strings.Contains(out, localVersion) {
			t.Fatalf("Local mode initial request failed: got %q err: %v", out, err)
		}

		// 2) Delete local – strict mode must NOT fall back
		cs.AppsV1().Deployments(ns).Delete(context.TODO(), localDeployName, metav1.DeleteOptions{})
		time.Sleep(5 * time.Second)

		out, err = shell.Execute(false,
			fmt.Sprintf("kubectl exec -n %s %s -- curl -m 5 -sS http://%s.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, serviceName, ns))
		if err == nil && strings.Contains(out, remoteVersion) {
			t.Fatalf("Local mode unexpectedly fell back to remote: %q", out)
		}
	})
}