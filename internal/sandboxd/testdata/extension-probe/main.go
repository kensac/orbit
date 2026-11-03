// Command extension-probe reports what an extension process can reach in its
// real sandboxd profile. It is built only for the Docker integration test.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type request struct {
	InvocationID string         `json:"invocation_id"`
	Parameters   map[string]any `json:"parameters"`
}

func main() {
	b, _ := io.ReadAll(os.Stdin)
	var req request
	_ = json.Unmarshal(b, &req)
	if req.Parameters["hang"] == true {
		_ = exec.Command("/bin/sh", "-c", "sleep 60").Start()
		select {}
	}
	result := inspect(req.Parameters)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"protocol": "orbit.extension.v1", "invocation_id": req.InvocationID,
		"status": "ok", "result": result,
	})
}

func inspect(params map[string]any) map[string]any {
	packagePath := "/work/extension/extension-probe"
	inputPath := "/work/inputs/data"
	packageBefore, _ := os.ReadFile(packagePath)
	inputBefore, _ := os.ReadFile(inputPath)
	packageRO := len(packageBefore) > 0 && os.WriteFile(packagePath, []byte("changed"), 0o600) != nil
	inputRO := string(inputBefore) == "probe-input" && os.WriteFile(inputPath, []byte("changed"), 0o600) != nil
	packageAfter, _ := os.ReadFile(packagePath)
	inputAfter, _ := os.ReadFile(inputPath)
	packageRO = packageRO && string(packageBefore) == string(packageAfter)
	inputRO = inputRO && string(inputBefore) == string(inputAfter)
	workRO := os.WriteFile("/work/root-write", []byte("x"), 0o600) != nil
	tmpWritable := os.WriteFile("/tmp/extension-probe", []byte("x"), 0o600) == nil
	outWritable := os.WriteFile("/work/out/extension-probe", []byte("x"), 0o600) == nil

	envClean, proxyAbsent, secretsAbsent := true, true, true
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if !map[string]bool{"HOME": true, "TMPDIR": true, "LANG": true, "PATH": true, "HOSTNAME": true}[key] {
			envClean = false
		}
		if strings.Contains(upper, "PROXY") {
			proxyAbsent = false
		}
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "ANTHROPIC") || strings.Contains(upper, "ORBIT") {
			secretsAbsent = false
		}
	}
	conn, err := net.DialTimeout("tcp", "1.1.1.1:443", 300*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, dnsErr := net.DefaultResolver.LookupHost(ctx, "example.com")
	cancel()
	_, sockErr := os.Stat("/var/run/docker.sock")
	status := readStatus()
	marker, _ := params["marker"].(string)
	other, _ := params["other"].(string)
	noCrossTmp := true
	if other != "" {
		_, err := os.Stat(filepath.Join("/tmp", other))
		noCrossTmp = os.IsNotExist(err)
	}
	if marker != "" {
		noCrossTmp = noCrossTmp && os.WriteFile(filepath.Join("/tmp", marker), []byte(marker), 0o600) == nil
	}
	return map[string]any{
		"package_read_only":    packageRO,
		"input_read_only":      inputRO,
		"work_root_read_only":  workRO,
		"tmp_writable":         tmpWritable,
		"out_writable":         outWritable,
		"no_network":           err != nil && dnsErr != nil,
		"proxy_absent":         proxyAbsent,
		"secrets_absent":       secretsAbsent,
		"environment_clean":    envClean,
		"docker_socket_absent": os.IsNotExist(sockErr),
		"capabilities_zero":    status["CapEff"] == "0000000000000000" && status["CapPrm"] == "0000000000000000" && status["CapBnd"] == "0000000000000000",
		"no_new_privileges":    status["NoNewPrivs"] == "1",
		"seccomp_enabled":      status["Seccomp"] == "2",
		"pids_max":             cgroupValue("pids.max", "/sys/fs/cgroup/pids/pids.max"),
		"memory_max":           cgroupValue("memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"),
		"no_cross_tmp":         noCrossTmp,
	}
}

func readStatus() map[string]string {
	b, _ := os.ReadFile("/proc/self/status")
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (key == "CapEff" || key == "CapPrm" || key == "CapBnd" || key == "NoNewPrivs" || key == "Seccomp") {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

func cgroupValue(paths ...string) string {
	for _, path := range paths {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return "unavailable"
}
