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

// NOTE: THE CODE IN THIS FILE IS MAINLY REFERENCED FROM ISTIO INTEGRATION
// FRAMEWORK(https://github.com/istio/istio/tree/master/tests/integration)
// AND ADAPTED FOR KMESH.

package kmesh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/util/traffic"
	kubetest "istio.io/istio/pkg/test/kube"
	"istio.io/istio/pkg/test/util/retry"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestKmeshRestart(t *testing.T) {
	framework.NewTest(t).Run(func(t framework.TestContext) {
		src := apps.EnrolledToKmesh[0]
		dst := apps.ServiceWithWaypointAtServiceGranularity
		options := echo.CallOptions{
			To:    dst,
			Count: 1,
			// Determine whether it is managed by Kmesh by passing through Waypoint.
			Check: httpValidator,
			Port: echo.Port{
				Name: "http",
			},
			Retry: echo.Retry{NoRetry: true},
		}

		g := traffic.NewGenerator(t, traffic.Config{
			Source:   src,
			Options:  options,
			Interval: 50 * time.Millisecond,
		}).Start()

		restartKmesh(t)

		g.Stop().CheckSuccessRate(t, 1)
	})
}

func restartKmesh(t framework.TestContext) {
	patchOpts := metav1.PatchOptions{}
	patchData := fmt.Sprintf(`{
			"spec": {
				"template": {
					"metadata": {
						"annotations": {
							"kubectl.kubernetes.io/restartedAt": %q
						}
					}
				}
			}
		}`, time.Now().Format(time.RFC3339))
	ds := t.Clusters().Default().Kube().AppsV1().DaemonSets(KmeshNamespace)
	_, err := ds.Patch(context.Background(), KmeshDaemonsetName, types.StrategicMergePatchType, []byte(patchData), patchOpts)
	if err != nil {
		t.Fatal(err)
	}

	if err := retry.UntilSuccess(func() error {
		d, err := ds.Get(context.Background(), KmeshDaemonsetName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !daemonsetsetComplete(d) {
			return fmt.Errorf("rollout is not yet done")
		}
		return nil
	}, retry.Timeout(60*time.Second), retry.Delay(2*time.Second)); err != nil {
		t.Fatal("failed to wait for Kmesh rollout status for: %v", err)
	}
	if _, err := kubetest.CheckPodsAreReady(kubetest.NewPodFetch(t.AllClusters()[0], KmeshNamespace, "app=kmesh")); err != nil {
		t.Fatal(err)
	}
}

func daemonsetsetComplete(ds *appsv1.DaemonSet) bool {
	return ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled && ds.Status.NumberReady == ds.Status.DesiredNumberScheduled && ds.Status.ObservedGeneration >= ds.Generation
}

func TestRestartService(t *testing.T) {
    // Interval between generated requests
    const callInterval = 100 * time.Millisecond
    // We expect 100% success rate if at least one instance remains available during restart.
    // (In a single-instance service, a brief failure may occur during downtime, but Kmesh should recover quickly.)
    successThreshold := 1.0

    framework.NewTest(t).Run(func(t framework.TestContext) {
        // Loop over each echo Instance in the "enrolled-to-kmesh" service (those managed by Kmesh).
        for _, dst := range apps.EnrolledToKmesh {
            // Prepare traffic generators for this destination.
            var generators []traffic.Generator

            // Define a helper to create and start a traffic generator from a given source to our destination.
            mkGenerator := func(src echo.Caller) {
                gen := traffic.NewGenerator(t, traffic.Config{
                    // Source is the client echo (calls will originate from this workload).
                    Source: src,
                    Options: echo.CallOptions{
                        // Destination is our target echo service instance.
                        To: dst,
                        Port: echo.Port{Name: "http"},          // use the HTTP port of the echo service
                        Path: "/?delay=10ms",                  // each request adds a 10ms server-side delay (to simulate processing)
                        Count: 1,                              // one request per interval tick
                        Retry: echo.Retry{NoRetry: true},      // no retry; we want to observe any failures directly
                        Check: check.OK(),                     // verify HTTP 200 OK response
                    },
                    Interval: callInterval,
                }).Start()  // start sending requests periodically
                generators = append(generators, gen)
            }

            // Choose a source for traffic. Here we use the same echo instance as both source and destination for simplicity.
            // (The echo component allows an instance to call itself via cluster IP, which will still be routed through Kmesh.)
            // Alternatively, we could use a separate client echo instance if available.
            mkGenerator(dst)

            // Trigger a restart of the destination service instance.
            // This will delete the pod and wait for a new pod to come up for the echo service.
            if err := dst.Restart(); err != nil {
                t.Fatal("failed to restart service: ", err)
            }

            // Stop the traffic generators and evaluate success rate of requests during the restart.
            for _, gen := range generators {
                // Stop the generator and compute the success ratio of its requests.
                genResult := gen.Stop()
                // Check that the success rate meets our threshold (ideally 100% if traffic was uninterrupted).
                genResult.CheckSuccessRate(t, successThreshold)
            }
        }
    })
}