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
// It first tries in-cluster config; if that fails, falls back to KUBECONFIG or ~/.kube/config.
func getK8sClient() (*kubernetes.Clientset, error) {
	if clientset != nil {
		return clientset, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig
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
	m := strings.TrimSpace(manifest)
	if m == "" {
		return nil
	}
	// Service?
	if strings.Contains(m, "kind: Service") {
		svc := &corev1.Service{}
		if err := yaml.Unmarshal([]byte(m), svc); err != nil {
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
	if strings.Contains(m, "kind: Deployment") {
		dep := &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(m), dep); err != nil {
			return err
		}
		dep.Namespace = namespace
		_, err = cs.AppsV1().Deployments(namespace).Create(context.TODO(), dep, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = cs.AppsV1().Deployments(namespace).Update(context.TODO(), dep, metav1.UpdateOptions{})
		}
		return err
	}
	return fmt.Errorf("unsupported manifest kind:\n%s", m)
}

// ensureNamespace makes sure the test namespace exists.
func ensureNamespace(ctx framework.TestContext) {
	cs, err := getK8sClient()
	if err != nil {
		ctx.Fatalf("getK8sClient failed: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{}); err != nil {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if _, err := cs.CoreV1().Namespaces().Create(context.TODO(), n, metav1.CreateOptions{}); err != nil {
			ctx.Fatalf("failed to create namespace %q: %v", ns, err)
		}
		ctx.Logf("Created namespace %q", ns)
	} else {
		ctx.Logf("Namespace %q already exists", ns)
	}
}

func TestLocalityLoadBalancing_PreferClose(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ensureNamespace(ctx)

		// 1) Service: PreferClose
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
		// 2) Local Deployment
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
		// 3) Remote Deployment
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
			ctx.Logf("Applying %s manifest", name)
			if err := applyManifest(ns, manifest); err != nil {
				ctx.Fatalf("applyManifest(%s) failed: %v", name, err)
			}
		}

		// Wait for pods ready
		if out, err := shell.Execute(true,
			fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=helloworld -n %s --timeout=120s", ns)); err != nil {
			ctx.Fatalf("waiting for helloworld pods failed: %v\n%s", err, out)
		}
		if out, err := shell.Execute(true,
			fmt.Sprintf("kubectl wait --for=condition=ready pod -l app=sleep -n %s --timeout=120s", ns)); err != nil {
			ctx.Fatalf("waiting for sleep pod failed: %v\n%s", err, out)
		}

		// Identify sleep pod
		cs, err := getK8sClient()
		if err != nil {
			ctx.Fatalf("getK8sClient failed: %v", err)
		}
		pods, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil || len(pods.Items) == 0 {
			ctx.Fatalf("failed to list sleep pod: %v", err)
		}
		sleepPod := pods.Items[0].Name

		// 1) PreferClose: expect local first
		ctx.Log("Verifying PreferClose: expecting local version")
		start := time.Now()
		for {
			out, err := shell.Execute(false,
				fmt.Sprintf("kubectl exec -n %s %s -- curl -s http://%s.%s.svc.cluster.local:5000/hello",
					ns, sleepPod, serviceName, ns))
			ctx.Logf("curl output: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, localVersion) {
				break
			}
			if time.Since(start) > 60*time.Second {
				ctx.Fatalf("PreferClose local timed out, last: %q err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}

		// 2) Delete local and verify failover
		ctx.Logf("Deleting local deployment %q", localDeployName)
		if err := cs.AppsV1().Deployments(ns).Delete(context.TODO(), localDeployName, metav1.DeleteOptions{}); err != nil {
			ctx.Fatalf("deleting local deployment failed: %v", err)
		}
		ctx.Log("Verifying PreferClose failover: expecting remote version")
		start = time.Now()
		for {
			out, err := shell.Execute(false,
				fmt.Sprintf("kubectl exec -n %s %s -- curl -s http://%s.%s.svc.cluster.local:5000/hello",
					ns, sleepPod, serviceName, ns))
			ctx.Logf("curl after delete: %q, err: %v", out, err)
			if err == nil && strings.Contains(out, remoteVersion) {
				break
			}
			if time.Since(start) > 60*time.Second {
				ctx.Fatalf("PreferClose failover timed out, last: %q err: %v", out, err)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func TestLocalityLoadBalancing_Local(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ensureNamespace(ctx)

		// Service: Local (strict)
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
		// Local, Remote, and Sleep manifests
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

		// Apply manifests
		for name, manifest := range map[string]string{
			"service": serviceYAML,
			"local":   depLocal,
			"remote":  depRemote,
			"client":  clientDep,
		} {
			ctx.Logf("Applying %s manifest", name)
			if err := applyManifest(ns, manifest); err != nil {
				ctx.Fatalf("applyManifest(%s) failed: %v", name, err)
			}
		}

		// Wait for readiness
		shell.Execute(true, fmt.Sprintf(
			"kubectl wait --for=condition=ready pod -l app=helloworld -n %s --timeout=120s", ns))
		shell.Execute(true, fmt.Sprintf(
			"kubectl wait --for=condition=ready pod -l app=sleep -n %s --timeout=120s", ns))

		// Identify sleep pod
		cs, err := getK8sClient()
		if err != nil {
			ctx.Fatalf("getK8sClient failed: %v", err)
		}
		pods, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil || len(pods.Items) == 0 {
			ctx.Fatalf("failed to list sleep pod: %v", err)
		}
		sleepPod := pods.Items[0].Name

		// Initial request — must hit local
		out, err := shell.Execute(false,
			fmt.Sprintf("kubectl exec -n %s %s -- curl -s http://%s.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, serviceName, ns))
		if err != nil || !strings.Contains(out, localVersion) {
			ctx.Fatalf("Local mode initial request failed: got %q err: %v", out, err)
		}

		// Delete local — no fallback
		if err := cs.AppsV1().Deployments(ns).Delete(context.TODO(), localDeployName, metav1.DeleteOptions{}); err != nil {
			ctx.Fatalf("deleting local deployment failed: %v", err)
		}
		time.Sleep(5 * time.Second)

		out, err = shell.Execute(false,
			fmt.Sprintf("kubectl exec -n %s %s -- curl -m 5 -s http://%s.%s.svc.cluster.local:5000/hello",
				ns, sleepPod, serviceName, ns))
		if err == nil && strings.Contains(out, remoteVersion) {
			ctx.Fatalf("Local mode unexpectedly fell back to remote: %q", out)
		}
	})
}