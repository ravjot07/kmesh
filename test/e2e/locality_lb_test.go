//go:build integ
// +build integ

/*
 * Copyright The Kmesh Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

 package kmesh

 import (
	 "context"
	 "os"
	 "strings"
	 "testing"
	 "time"
 
	 "istio.io/istio/pkg/test/framework"
	 "istio.io/istio/pkg/test/shell"
	 "istio.io/istio/pkg/test/util/retry"
 )
 
 // applyManifest writes the provided manifest into a temporary file and applies it using kubectl.
 func applyManifest(ns, manifest string) error {
	 tmpFile, err := os.CreateTemp("", "manifest-*.yaml")
	 if err != nil {
		 return err
	 }
	 defer os.Remove(tmpFile.Name())
 
	 if _, err := tmpFile.WriteString(manifest); err != nil {
		 tmpFile.Close()
		 return err
	 }
	 tmpFile.Close()
 
	 cmd := "kubectl apply -n " + ns + " -f " + tmpFile.Name()
	 _, err = shell.Execute(true, cmd)
	 return err
 }
 
 // labelNode applies a label to the specified node.
 func labelNode(node, key, value string) error {
	 cmd := "kubectl label node " + node + " " + key + "=" + value + " --overwrite"
	 _, err := shell.Execute(true, cmd)
	 return err
 }
 
 // getTwoNodes returns two node names: prefers a worker for "local" and then either a second worker or the control-plane for "remote".
 func getTwoNodes(ctx context.Context) (local, remote string, err error) {
	 // Use a simple kubectl command to list node names.
	 out, err := shell.Execute(true, "kubectl get nodes -o jsonpath='{.items[*].metadata.name}'")
	 if err != nil {
		 return "", "", err
	 }
	 nodes := strings.Fields(out)
	 if len(nodes) < 2 {
		 return "", "", err
	 }
	 // In many CI clusters, the worker is not tainted while the control-plane is.
	 // For simplicity, choose the first node as local and the second as remote.
	 local = nodes[0]
	 remote = nodes[1]
	 return local, remote, nil
 }
 
 func TestPreferCloseLocalityLB(t *testing.T) {
	 // Skip the test if ISTIO_VERSION is below 1.23.1.
	 if v := os.Getenv("ISTIO_VERSION"); v != "" {
		 if strings.HasPrefix(v, "1.22") || strings.HasPrefix(v, "1.21") {
			 t.Skipf("Skipping PreferClose locality LB test: ISTIO_VERSION=%s does not support the feature", v)
		 }
	 }
 
	 framework.NewTest(t).Run(func(t framework.TestContext) {
		 const ns = "sample"
 
		 // Create test namespace.
		 if _, err := shell.Execute(true, "kubectl create namespace "+ns); err != nil {
			 t.Logf("Namespace %s may already exist: %v", ns, err)
		 }
 
		 // Dynamically determine two nodes.
		 localNode, remoteNode, err := getTwoNodes(context.Background())
		 if err != nil {
			 t.Skipf("Skipping test: could not retrieve two nodes: %v", err)
		 }
		 t.Logf("Using local node: %s, remote node: %s", localNode, remoteNode)
 
		 // Label the nodes for test scheduling.
		 if err := labelNode(localNode, "test/locality", "local"); err != nil {
			 t.Fatalf("Failed to label node %s: %v", localNode, err)
		 }
		 if err := labelNode(remoteNode, "test/locality", "remote"); err != nil {
			 t.Fatalf("Failed to label node %s: %v", remoteNode, err)
		 }
 
		 // Ensure cleanup of node labels after test.
		 defer func() {
			 shell.Execute(true, "kubectl label node "+localNode+" test/locality-")
			 shell.Execute(true, "kubectl label node "+remoteNode+" test/locality-")
		 }()
 
		 // Label namespace to enable ambient mode if necessary.
		 _, _ = shell.Execute(true, "kubectl label namespace "+ns+" istio.io/dataplane-mode=ambient --overwrite")
 
		 // Apply Service manifest with PreferClose annotation.
		 serviceYAML := `
 apiVersion: v1
 kind: Service
 metadata:
   name: helloworld
   namespace: sample
   annotations:
	 networking.istio.io/traffic-distribution: "PreferClose"
 spec:
   selector:
	 app: helloworld
   ports:
   - port: 5000
	 name: http
	 targetPort: 5000
 `
		 if err := applyManifest(ns, serviceYAML); err != nil {
			 t.Fatalf("Failed to apply Service manifest: %v", err)
		 }
 
		 // Deploy local instance.
		 localDepYAML := `
 apiVersion: apps/v1
 kind: Deployment
 metadata:
   name: helloworld-local
   namespace: sample
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
	   nodeSelector:
		 test/locality: "local"
	   containers:
	   - name: helloworld
		 image: docker.io/istio/examples-helloworld-v1
		 imagePullPolicy: IfNotPresent
		 env:
		 - name: SERVICE_VERSION
		   value: "region.zone1.subzone1"
		 ports:
		 - containerPort: 5000
 `
		 if err := applyManifest(ns, localDepYAML); err != nil {
			 t.Fatalf("Failed to deploy local instance: %v", err)
		 }
 
		 // Deploy remote instance.
		 remoteDepYAML := `
 apiVersion: apps/v1
 kind: Deployment
 metadata:
   name: helloworld-remote
   namespace: sample
   labels:
	 app: helloworld
	 version: region.zone2.subzone3
 spec:
   replicas: 1
   selector:
	 matchLabels:
	   app: helloworld
	   version: region.zone2.subzone3
   template:
	 metadata:
	   labels:
		 app: helloworld
		 version: region.zone2.subzone3
	 spec:
	   nodeSelector:
		 test/locality: "remote"
	   tolerations:
	   - key: "node-role.kubernetes.io/control-plane"
		 operator: "Exists"
		 effect: "NoSchedule"
	   - key: "node-role.kubernetes.io/master"
		 operator: "Exists"
		 effect: "NoSchedule"
	   containers:
	   - name: helloworld
		 image: docker.io/istio/examples-helloworld-v1
		 imagePullPolicy: IfNotPresent
		 env:
		 - name: SERVICE_VERSION
		   value: "region.zone2.subzone3"
		 ports:
		 - containerPort: 5000
 `
		 if err := applyManifest(ns, remoteDepYAML); err != nil {
			 t.Fatalf("Failed to deploy remote instance: %v", err)
		 }
 
		 // Deploy a client pod on the local node.
		 clientPodYAML := `
 apiVersion: v1
 kind: Pod
 metadata:
   name: curl-client
   namespace: sample
   labels:
	 app: curl-client
 spec:
   nodeSelector:
	 test/locality: "local"
   restartPolicy: Never
   containers:
   - name: curl
	 image: curlimages/curl:latest
	 command: ["sleep", "3600"]
 `
		 if err := applyManifest(ns, clientPodYAML); err != nil {
			 t.Fatalf("Failed to deploy curl client: %v", err)
		 }
 
		 // Wait for Deployments and client pod to become ready.
		 deployments := []string{"helloworld-local", "helloworld-remote"}
		 for _, dep := range deployments {
			 cmd := "kubectl wait --for=condition=available deployment/" + dep + " -n " + ns + " --timeout=120s"
			 if _, err := shell.Execute(true, cmd); err != nil {
				 t.Fatalf("Deployment %s not ready: %v", dep, err)
			 }
		 }
		 // Wait for the client pod.
		 if _, err := shell.Execute(true, "kubectl wait --for=condition=ready pod/curl-client -n "+ns+" --timeout=120s"); err != nil {
			 t.Fatalf("curl-client pod not ready: %v", err)
		 }
 
		 // Test Locality Preferred: From the curl client, expect response from local instance.
		 t.Log("Testing locality preferred: expecting response from region.zone1.subzone1")
		 var localResponse string
		 if err := retry.Until(func() bool {
			 out, execErr := shell.Execute(true,
				 "kubectl exec -n "+ns+" curl-client -c curl -- curl -sSL http://helloworld:5000/hello")
			 if execErr != nil {
				 t.Logf("Curl exec error: %v", execErr)
				 return false
			 }
			 t.Logf("Curl output: %s", out)
			 if strings.Contains(out, "region.zone1.subzone1") {
				 localResponse = out
				 return true
			 }
			 return false
		 }, retry.Timeout(60*time.Second), retry.Delay(2*time.Second)); err != nil {
			 t.Fatalf("Locality preferred test failed: expected response containing 'region.zone1.subzone1', got: %s", localResponse)
		 }
		 t.Log("Locality preferred test passed.")
 
		 // Test Locality Failover: Delete the local instance and expect remote instance to serve traffic.
		 t.Log("Testing locality failover: deleting local instance to expect response from region.zone2.subzone3")
		 if _, err := shell.Execute(true, "kubectl delete deployment helloworld-local -n "+ns); err != nil {
			 t.Fatalf("Failed to delete local instance: %v", err)
		 }
		 // Wait for endpoint update propagation.
		 time.Sleep(10 * time.Second)
 
		 var remoteResponse string
		 if err := retry.Until(func() bool {
			 out, execErr := shell.Execute(true,
				 "kubectl exec -n "+ns+" curl-client -c curl -- curl -sSL http://helloworld:5000/hello")
			 if execErr != nil {
				 t.Logf("Curl exec error after failover: %v", execErr)
				 return false
			 }
			 t.Logf("Curl output after failover: %s", out)
			 if strings.Contains(out, "region.zone2.subzone3") {
				 remoteResponse = out
				 return true
			 }
			 return false
		 }, retry.Timeout(60*time.Second), retry.Delay(2*time.Second)); err != nil {
			 t.Fatalf("Locality failover test failed: expected response containing 'region.zone2.subzone3', got: %s", remoteResponse)
		 }
		 t.Log("Locality failover test passed.")
	 })
 }
 