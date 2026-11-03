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
		time.Sleep(time.Minute)
	}
	if req.Parameters["memory"] == true {
		// Keep allocating until the container's cgroup limit kills this process.
		// The caller expects an invocation failure and verifies sandbox cleanup.
		var held [][]byte
		for {
			held = append(held, make([]byte, 8<<20))
			for i := range held[len(held)-1] {
				held[len(held)-1][i] = 1
			}
		}
	}
	result := inspect(req.Parameters)
	if req.Parameters["flood"] == true {
		for key, value := range exhaustProcesses() {
			result[key] = value
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"protocol": "orbit.extension.v1", "invocation_id": req.InvocationID,
		"status": "ok", "result": result,
	})
}

func inspect(params map[string]any) map[string]any {
	packagePath := "/orbit-extension/extension/extension-probe"
	inputPath := "/orbit-extension/inputs/data"
	packageBefore, _ := os.ReadFile(packagePath)
	inputBefore, inputErr := os.ReadFile(inputPath)
	packageRO := len(packageBefore) > 0 && os.WriteFile(packagePath, []byte("changed"), 0o600) != nil
	inputWriteErr := os.WriteFile(inputPath, []byte("changed"), 0o600)
	packageAfter, _ := os.ReadFile(packagePath)
	inputAfter, _ := os.ReadFile(inputPath)
	packageRO = packageRO && string(packageBefore) == string(packageAfter)
	inputRO := inputErr == nil && string(inputBefore) == "probe-input" && inputWriteErr != nil && string(inputBefore) == string(inputAfter)
	workWriteErr := os.WriteFile("/orbit-extension/root-write", []byte("x"), 0o600)
	workChmodErr := os.Chmod("/orbit-extension", 0o755)
	workRO := workWriteErr != nil && workChmodErr != nil
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
	hostPathsUnreadable := true
	for _, path := range []string{"/proc/1/root/etc/shadow", "/root/.ssh", "/run/secrets", "/home/agent/.claude", "/work/.orbit/claude", "/host", "/mnt/host"} {
		b, readErr := os.ReadFile(path)
		entries, dirErr := os.ReadDir(path)
		if readErr == nil && len(b) > 0 || dirErr == nil && len(entries) > 0 {
			hostPathsUnreadable = false
		}
	}
	linkEscapeBlocked := false
	if err := os.Symlink("/etc", "/work/out/escape"); err == nil {
		linkEscapeBlocked = os.WriteFile("/work/out/escape/orbit-probe", []byte("x"), 0o600) != nil
		_ = os.Remove("/work/out/escape")
	}
	hardlinkEscapeBlocked := os.Link(inputPath, "/work/out/input-hardlink") != nil
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
		"package_read_only":       packageRO,
		"input_present":           inputErr == nil,
		"input_read_only":         inputRO,
		"input_write_error":       errorText(inputWriteErr),
		"work_root_read_only":     workRO,
		"work_root_write_error":   errorText(workWriteErr),
		"work_root_chmod_error":   errorText(workChmodErr),
		"tmp_writable":            tmpWritable,
		"out_writable":            outWritable,
		"no_network":              err != nil && dnsErr != nil,
		"proxy_absent":            proxyAbsent,
		"secrets_absent":          secretsAbsent,
		"environment_clean":       envClean,
		"docker_socket_absent":    os.IsNotExist(sockErr),
		"capabilities_zero":       status["CapEff"] == "0000000000000000" && status["CapPrm"] == "0000000000000000" && status["CapBnd"] == "0000000000000000",
		"no_new_privileges":       status["NoNewPrivs"] == "1",
		"seccomp_enabled":         status["Seccomp"] == "2",
		"host_paths_unreadable":   hostPathsUnreadable,
		"link_escape_blocked":     linkEscapeBlocked,
		"hardlink_escape_blocked": hardlinkEscapeBlocked,
		"fork_limit_hit":          false,
		"forks_before_limit":      0,
		"no_cross_tmp":            noCrossTmp,
	}
}

func exhaustProcesses() map[string]any {
	var children []*exec.Cmd
	for {
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			for _, running := range children {
				_ = running.Process.Kill()
				_ = running.Wait()
			}
			return map[string]any{"forks_before_limit": len(children), "fork_limit_hit": true}
		}
		children = append(children, child)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
