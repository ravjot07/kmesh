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
	"testing"
	"time"
	"strings"
	"os"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/rest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/shell"
)

const ns = "sample"

// k8sClient is a Kubernetes client for applying manifests and querying resources.
var k8sClient *kubernetes.Clientset

// getK8sClient initializes and returns a Kubernetes clientset, using in-cluster config if available or KUBECONFIG if set.
func getK8sClient() (*kubernetes.Clientset, error) {
	if k8sClient != nil {
		return k8sClient, nil
	}
	var config *rest.Config
	var err error
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// 2) Fallback to kubeconfig (KUBECONFIG env var or default path)
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %v", err)
		}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}
	k8sClient = client
	return k8sClient, nil
}

// applyManifest creates or updates a Kubernetes resource from the given YAML manifest in the specified namespace.
func applyManifest(namespace string, manifest string) error {
	client, err := getK8sClient()
	if err != nil {
		return err
	}
	// Decode manifest to determine kind
	manifest = strings.TrimSpace(manifest)
	if manifest == "" {
		return nil
	}
	switch {
	case strings.Contains(manifest, "kind: Service"):
		// Create or update Service
		svc := manifestToService(manifest)
		svc.Namespace = namespace
		_, err := client.CoreV1().Services(namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = client.CoreV1().Services(namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
		}
		return err
	case strings.Contains(manifest, "kind: Deployment"):
		// Create or update Deployment
		dep := manifestToDeployment(manifest)
		dep.Namespace = namespace
		_, err := client.AppsV1().Deployments(namespace).Create(context.TODO(), dep, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "AlreadyExists") {
			_, err = client.AppsV1().Deployments(namespace).Update(context.TODO(), dep, metav1.UpdateOptions{})
		}
		return err
	default:
		return fmt.Errorf("unsupported manifest kind or format")
	}
}

// manifestToService decodes a Service manifest YAML string into a Service object.
func manifestToService(manifest string) *corev1.Service {
	svc := &corev1.Service{}
	_ = yaml.Unmarshal([]byte(manifest), svc)
	return svc
}

// manifestToDeployment decodes a Deployment manifest YAML string into a Deployment object.
func manifestToDeployment(manifest string) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	_ = yaml.Unmarshal([]byte(manifest), dep)
	return dep
}

func TestLocalityLoadBalancing_PreferClose(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		// Define the Service and Deployments with trafficDistribution: PreferClose
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
        imagePullPolicy: IfNotPresent
        command: ["/bin/sleep", "infinity"]
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		// Apply manifests
		if err := applyManifest(ns, serviceYAML); err != nil {
			t.Fatalf("Failed to apply Service manifest: %v", err)
		}
		if err := applyManifest(ns, depLocal); err != nil {
			t.Fatalf("Failed to deploy local instance: %v", err)
		}
		if err := applyManifest(ns, depRemote); err != nil {
			t.Fatalf("Failed to deploy remote instance: %v", err)
		}
		if err := applyManifest(ns, clientDep); err != nil {
			t.Fatalf("Failed to deploy sleep client: %v", err)
		}
		// Wait for all deployments to be ready
		if _, err := shell.Execute(true, "kubectl wait --for=condition=available deployment -n " + ns + " --timeout=120s --all"); err != nil {
			t.Fatalf("Deployments not ready: %v", err)
		}
		// Get name of the sleep client pod
		client, err := getK8sClient()
		if err != nil {
			t.Fatalf("Failed to get k8s client: %v", err)
		}
		podList, err := client.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil || len(podList.Items) == 0 {
			t.Fatalf("Failed to get sleep pod: %v", err)
		}
		sleepPod := podList.Items[0].Name

		// Test Locality Preference (PreferClose): expect response from local instance
		t.Log("Testing locality preferred: expecting response from local zone...")
		var localResponse string
		retryTimeout := 60 * time.Second
		start := time.Now()
		for {
			out, err := shell.Execute(false, "kubectl exec -n "+ns+" "+sleepPod+" -- curl -sS http://helloworld."+ns+".svc.cluster.local:5000/hello")
			if err != nil {
				t.Logf("Curl error: %v", err)
				// continue retrying until timeout
			} else {
				t.Logf("Curl output: %s", out)
				if strings.Contains(out, "region.zone1.subzone1") {
					localResponse = out
					break
				}
			}
			if time.Since(start) > retryTimeout {
				t.Fatalf("Locality preferred test failed: expected response from region.zone1.subzone1, got: %s", localResponse)
			}
			time.Sleep(2 * time.Second)
		}
		t.Log("Locality preferred (PreferClose) test passed: traffic served by local instance.")

		// Test Locality Failover: delete local instance and expect remote instance to serve traffic
		t.Log("Testing locality failover: expecting response from remote zone after local removal...")
		// Delete the local instance deployment
		if err := client.AppsV1().Deployments(ns).Delete(context.TODO(), "helloworld-region-zone1-subzone1", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Failed to delete local instance deployment: %v", err)
		}
		var failoverResponse string
		start = time.Now()
		for {
			out, err := shell.Execute(false, "kubectl exec -n "+ns+" "+sleepPod+" -- curl -sS http://helloworld."+ns+".svc.cluster.local:5000/hello")
			if err != nil {
				t.Logf("Curl error after failover: %v", err)
			} else {
				t.Logf("Curl output after failover: %s", out)
				if strings.Contains(out, "region.zone1.subzone2") {
					failoverResponse = out
					break
				}
			}
			if time.Since(start) > retryTimeout {
				t.Fatalf("Locality failover test failed: expected response from region.zone1.subzone2, got: %s", failoverResponse)
			}
			time.Sleep(2 * time.Second)
		}
		t.Log("Locality failover (PreferClose) test passed: traffic fell back to remote instance.")
	})
}

func TestLocalityLoadBalancing_Local(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		// Define Service and Deployments with trafficDistribution: Local (strict locality)
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
        imagePullPolicy: IfNotPresent
        command: ["/bin/sleep", "infinity"]
      nodeSelector:
        kubernetes.io/hostname: kmesh-testing-worker
`
		// Apply manifests
		if err := applyManifest(ns, serviceYAML); err != nil {
			t.Fatalf("Failed to apply Service manifest: %v", err)
		}
		if err := applyManifest(ns, depLocal); err != nil {
			t.Fatalf("Failed to deploy local instance: %v", err)
		}
		if err := applyManifest(ns, depRemote); err != nil {
			t.Fatalf("Failed to deploy remote instance: %v", err)
		}
		if err := applyManifest(ns, clientDep); err != nil {
			t.Fatalf("Failed to deploy sleep client: %v", err)
		}
		// Wait for all deployments to be ready
		if _, err := shell.Execute(true, "kubectl wait --for=condition=available deployment -n " + ns + " --timeout=120s --all"); err != nil {
			t.Fatalf("Deployments not ready: %v", err)
		}
		// Get sleep pod name
		client, err := getK8sClient()
		if err != nil {
			t.Fatalf("Failed to get k8s client: %v", err)
		}
		podList, err := client.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: "app=sleep"})
		if err != nil || len(podList.Items) == 0 {
			t.Fatalf("Failed to get sleep pod: %v", err)
		}
		sleepPod := podList.Items[0].Name

		// Test Locality Strict mode: expect only local endpoint is used
		t.Log("Testing strict locality: expecting traffic only to local endpoint...")
		// Initial request to confirm local endpoint responds
		out, err := shell.Execute(false, "kubectl exec -n " + ns + " " + sleepPod + " -- curl -sS http://helloworld." + ns + ".svc.cluster.local:5000/hello")
		if err != nil {
			t.Fatalf("Initial curl in Local mode failed: %v", err)
		}
		if !strings.Contains(out, "region.zone1.subzone1") {
			t.Fatalf("Strict locality test failed: expected local instance response, got: %s", out)
		}
		t.Logf("Received local response: %s", out)
		// Delete local instance
		if err := client.AppsV1().Deployments(ns).Delete(context.TODO(), "helloworld-region-zone1-subzone1", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Failed to delete local instance deployment: %v", err)
		}
		// Allow time for Kmesh to update endpoints (local removed)
		time.Sleep(5 * time.Second)
		// Attempt to curl service again; expect failure or no response (no fallback in strict mode)
		out, err = shell.Execute(false, "kubectl exec -n " + ns + " " + sleepPod + " -- curl -m 5 -sS http://helloworld." + ns + ".svc.cluster.local:5000/hello")
		if err == nil {
			// If curl succeeded, check that it is NOT a remote response
			if strings.Contains(out, "region.zone1.subzone2") {
				t.Fatalf("Strict locality mode failed: got remote response %q, but no remote fallback should occur", out)
			}
			if strings.Contains(out, "region.zone1.subzone1") {
				t.Fatalf("Strict locality mode: local instance response still received after deletion (pod may not have terminated yet): %s", out)
			}
			// Any other successful output is unexpected
			t.Fatalf("Strict locality mode: unexpected success response: %s", out)
		}
		t.Log("Locality Local mode test passed: no remote endpoint was used after local endpoint removal.")
	})
}
