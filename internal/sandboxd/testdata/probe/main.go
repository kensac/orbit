// Command probe runs inside a build sandbox in the integration test and
// reports, as JSON, what the process can and cannot do there.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	out := map[string]any{
		"uid": os.Getuid(),
		"gid": os.Getgid(),
	}
	status := map[string]string{}
	if f, err := os.Open("/proc/self/status"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), ":"); ok {
				switch k {
				case "CapEff", "CapPrm", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp":
					status[k] = strings.TrimSpace(v)
				}
			}
		}
		f.Close()
	}
	out["status"] = status

	writes := map[string]string{}
	for _, p := range []string{"/etc/probe", "/probe", "/usr/local/bin/probe2", "/var/probe", "/root/probe", "/tmp/probe", "/work/probe.txt"} {
		err := os.WriteFile(p, []byte("x"), 0o644)
		writes[p] = errString(err)
	}
	out["writes"] = writes

	dials := map[string]string{}
	for _, a := range []string{"1.1.1.1:443", "8.8.8.8:53", "140.82.112.3:443"} {
		c, err := net.DialTimeout("tcp", a, 3*time.Second)
		if err == nil {
			c.Close()
		}
		dials[a] = errString(err)
	}
	out["dials"] = dials

	_, err := net.LookupHost("example.com")
	out["dns"] = errString(err)

	proxy := map[string]any{}
	if u, err := url.Parse(os.Getenv("HTTPS_PROXY")); err == nil && u.Host != "" {
		for _, target := range []string{"github.com:443", "api.github.com:443", "example.com:443", "proxy.golang.org:443", "proxy.golang.org:80"} {
			proxy[target] = connect(u.Host, target)
		}
	}
	out["proxy"] = proxy

	_, err = os.Stat("/var/run/docker.sock")
	out["docker_sock"] = errString(err)

	var names []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	sort.Strings(names)
	out["env"] = names

	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func connect(proxy, target string) any {
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return "dial proxy: " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return "read: " + err.Error()
	}
	return resp.StatusCode
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
