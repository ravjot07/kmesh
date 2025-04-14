//go:build integ
// +build integ

/*
End-to-End Test for kmeshctl authz Commands in Kmesh.
This test performs the following steps:
1. Automatically retrieves a running Kmesh Daemon pod from the "kmesh-system" namespace.
2. Waits for the pod to become ready.
3. Enables authorization offloading using "kmeshctl authz enable <pod>".
4. Verifies the status using "kmeshctl authz status <pod>" (expecting enabled output).
5. Disables authorization using "kmeshctl authz disable <pod>".
6. Verifies the status again (expecting disabled output).
This test ensures that the authz commands work correctly on a live cluster.
*/

package kmesh

import (
    "bytes"
    "os/exec"
    "strings"
    "testing"
    "time"
)

// TestKmeshctlAuthzCommands verifies that the kmeshctl authz enable/disable commands 
// correctly toggle the L4 authorization offloading and that the status subcommand 
// reflects the changes. It requires a running Kmesh daemon pod and the kmeshctl binary in PATH.
func TestKmeshctlAuthzCommands(t *testing.T) {
    // Define the namespace and label to locate the Kmesh daemon pod. 
    // (Assuming Kmesh is deployed in namespace "kmesh-system" and labeled "app=kmesh-daemon"; 
    // adjust these values if different in the deployment.)
    const kmeshNamespace = "kmesh-system"
    const kmeshLabelSelector = "app=kmesh-daemon"

    // Step 1: Find the name of a running Kmesh daemon pod.
    var podName string
    {
        // Use kubectl to get the first pod name matching the Kmesh daemon label.
        cmd := exec.Command("kubectl", "-n", kmeshNamespace, "get", "pods",
            "-l", kmeshLabelSelector, "-o", "jsonpath={.items[0].metadata.name}")
        output, err := cmd.Output()
        if err != nil || len(output) == 0 {
            t.Fatalf("Failed to find Kmesh daemon pod (namespace=%s, label=%s): %v", 
                     kmeshNamespace, kmeshLabelSelector, err)
        }
        podName = string(output)
        t.Logf("Found Kmesh daemon pod: %s", podName)
    }

    // Step 2: Wait until the Kmesh daemon pod is in Running/Ready state before proceeding.
    {
        const maxRetries = 30
        const delay = 2 * time.Second
        var podReady bool
        for i := 0; i < maxRetries; i++ {
            // Check pod phase using kubectl (could also check condition Ready).
            cmd := exec.Command("kubectl", "-n", kmeshNamespace, "get", "pod", podName, "-o", "jsonpath={.status.phase}")
            phaseOut, err := cmd.Output()
            phase := string(bytes.TrimSpace(phaseOut))
            if err == nil && strings.EqualFold(phase, "Running") {
                podReady = true
                break
            }
            time.Sleep(delay)
        }
        if !podReady {
            t.Fatalf("Kmesh daemon pod %s is not running/ready after waiting", podName)
        }
        t.Logf("Kmesh daemon pod %s is running and ready.", podName)
    }

    // Step 3: (Optional) Check initial authz status (expect disabled by default).
    {
        cmd := exec.Command("kmeshctl", "authz", "status", podName)
        output, err := cmd.CombinedOutput()
        if err != nil {
            // Non-zero exit could indicate the command failed (if feature not present or other error).
            t.Fatalf("Initial 'kmeshctl authz status' failed: %v, output: %s", err, string(output))
        }
        // Trim and normalize the output for comparison.
        status := strings.TrimSpace(string(output))
        t.Logf("Initial authz status output: %q", status)
        // We expect "false" or "disabled" (depending on implementation) when authz is off.
        // Accept "false" or "disabled" as indicating not enabled.
        if strings.EqualFold(status, "false") || strings.EqualFold(status, "disabled") {
            t.Log("Authz is initially disabled (as expected).")
        } else if strings.EqualFold(status, "true") || strings.EqualFold(status, "enabled") {
            t.Log("Authz appears to be initially enabled (unexpected, but continuing).")
        } else {
            t.Errorf("Unexpected initial authz status output: %q (expected 'false'/'disabled')", status)
        }
    }

    // Step 4: Enable authz on the Kmesh daemon pod using kmeshctl.
    t.Run("enable-authz", func(t *testing.T) {
        cmd := exec.Command("kmeshctl", "authz", "enable", podName)
        output, err := cmd.CombinedOutput()
        t.Logf("Output of 'kmeshctl authz enable': %s", output)
        if err != nil {
            t.Fatalf("Failed to enable authz via kmeshctl: %v (output: %s)", err, string(output))
        }
        // If the enable command produces a specific success message or no output, we can check that here.
        // (For example, if it prints "Authorization enabled" or returns an empty output on success.)
    })

    // Step 5: Verify that authz status is now enabled (true).
    t.Run("status-authz-enabled", func(t *testing.T) {
        cmd := exec.Command("kmeshctl", "authz", "status", podName)
        output, err := cmd.CombinedOutput()
        if err != nil {
            t.Fatalf("Failed to get authz status after enabling: %v, output: %s", err, string(output))
        }
        status := strings.TrimSpace(string(output))
        t.Logf("Authz status after enabling: %q", status)
        // Expect "true" or "enabled" to indicate authz is now active.
        if strings.EqualFold(status, "true") || strings.EqualFold(status, "enabled") {
            // Success case: authz is enabled
        } else {
            t.Errorf("Authz status did not report enabled after enabling. Output: %q", status)
        }
    })

    // Step 6: Disable authz on the Kmesh daemon pod.
    t.Run("disable-authz", func(t *testing.T) {
        cmd := exec.Command("kmeshctl", "authz", "disable", podName)
        output, err := cmd.CombinedOutput()
        t.Logf("Output of 'kmeshctl authz disable': %s", output)
        if err != nil {
            t.Fatalf("Failed to disable authz via kmeshctl: %v (output: %s)", err, string(output))
        }
        // (Optionally check for expected success message or output if any.)
    })

    // Step 7: Verify that authz status is now disabled (false).
    t.Run("status-authz-disabled", func(t *testing.T) {
        cmd := exec.Command("kmeshctl", "authz", "status", podName)
        output, err := cmd.CombinedOutput()
        if err != nil {
            t.Fatalf("Failed to get authz status after disabling: %v, output: %s", err, string(output))
        }
        status := strings.TrimSpace(string(output))
        t.Logf("Authz status after disabling: %q", status)
        // Expect "false" or "disabled" now.
        if strings.EqualFold(status, "false") || strings.EqualFold(status, "disabled") {
            // Success: authz is disabled as expected.
        } else {
            t.Errorf("Authz status did not report disabled after disabling. Output: %q", status)
        }
    })
}
