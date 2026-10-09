/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/bouncer"
	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The publishing proxy's configuration, run by the proxy itself.
//
// What the configuration is for cannot be read off its text: whether a path
// with an encoded slash leaves a declared prefix is a question about how
// nginx reads a request line, and the only honest answer is nginx's. So
// these start the image the operator deploys, with the configuration it
// renders, in front of a server that says back what it was sent, and ask.
//
// They need docker and the image. Without docker they are skipped; without
// the image they are skipped unless CI is set, where the image is pulled
// (and skipped again, loudly, if the registry refuses).

type perimeterRig struct {
	t    *testing.T
	addr string
}

// perimeterEcho is the application's stand-in: it answers every request with
// the path and the headers it was sent, one per line, and with the headers
// an application must not get out past the proxy.
func perimeterEcho(headers []string) string {
	var b strings.Builder
	b.WriteString("worker_processes 1;\npid /tmp/nginx.pid;\nerror_log /dev/stderr warn;\nevents { worker_connections 256; }\n")
	b.WriteString("http {\n  access_log off;\n  client_body_temp_path /tmp/cb;\n  proxy_temp_path /tmp/p;\n  fastcgi_temp_path /tmp/f;\n  uwsgi_temp_path /tmp/u;\n  scgi_temp_path /tmp/s;\n")
	b.WriteString("  underscores_in_headers on;\n  ignore_invalid_headers off;\n  client_max_body_size 0;\n")
	b.WriteString("  server {\n    listen 8080;\n    location / {\n")
	b.WriteString("      add_header Set-Cookie \"app=1; Path=/\" always;\n")
	b.WriteString("      add_header X-Powered-By \"the-application\" always;\n")
	b.WriteString("      default_type text/plain;\n")
	body := "REACHED\\nuri=$request_uri\\nmethod=$request_method\\nhost=$http_host\\n"
	for _, h := range headers {
		body += fmt.Sprintf("%s=[$http_%s]\\n", strings.ToLower(h), strings.ToLower(strings.ReplaceAll(h, "-", "_")))
	}
	fmt.Fprintf(&b, "      return 200 \"%s\";\n", body)
	b.WriteString("    }\n  }\n}\n")
	return b.String()
}

// seenHeaders are what the echo reports of a request.
func seenHeaders() []string {
	out := []string{
		"Cookie", "Authorization", "X-Forwarded-For", "X-Real-IP", "X-Forwarded-Proto", "X-Forwarded-Host",
		"Forwarded", "X-Forwarded-Port", "X-Forwarded-Prefix", "X-Forwarded-Server", "X-Original-URL", "X-Rewrite-URL",
		"X-Forwarded-Access-Token",
	}
	return append(out, perimeterStrippedIdentityHeaders()...)
}

func startPerimeterRig(t *testing.T, e *gentianov1alpha1.ExposureSpec, website bool, limits perimeterLimits) *perimeterRig {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed: the proxy's configuration was not run by the proxy")
	}
	image := kernel.DefaultPerimeterProxyImage
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("image %s is not present (docker pull %s): the proxy's configuration was not run by the proxy", image, image)
		}
		if out, err := exec.Command("docker", "pull", "-q", image).CombinedOutput(); err != nil {
			t.Skipf("image %s could not be pulled, so the proxy's configuration was not run by the proxy: %s", image, out)
		}
	}

	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	network, echo, proxy := "perimeter-test-"+id, "perimeter-echo-"+id, "perimeter-proxy-"+id
	dir := t.TempDir()
	// The containers run as nginx's own user, as the pod does, and must be
	// able to read what the test wrote.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	echoConf := write("echo.conf", perimeterEcho(seenHeaders()))
	proxyConf := write("proxy.conf", perimeterProxyConfig(e, echo, 8080, website, e.AuthMode == gentianov1alpha1.AuthModeApp, limits))

	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("network", "create", network)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", proxy, echo).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})
	// As the Deployment runs it: nginx's user, a read-only root, /tmp to
	// write in, and the configuration in nginx's own place.
	run := func(name, conf string, extra ...string) {
		args := []string{"run", "-d", "--name", name, "--network", network, "--user", "101", "--read-only",
			"--tmpfs", "/tmp:uid=101", "-v", conf + ":/etc/nginx/nginx.conf:ro"}
		args = append(args, extra...)
		docker(append(args, image)...)
	}
	run(echo, echoConf)
	run(proxy, proxyConf, "-p", "127.0.0.1::8080")
	port := docker("port", proxy, "8080/tcp")
	rig := &perimeterRig{t: t, addr: strings.TrimSpace(strings.Split(port, "\n")[0])}

	deadline := time.Now().Add(20 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", rig.addr, time.Second); err == nil {
			_ = conn.Close()
			if status, _, _ := rig.raw("GET /__ready HTTP/1.1\r\nHost: ready\r\nConnection: close\r\n\r\n"); status != 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", proxy).CombinedOutput()
			t.Fatalf("the proxy did not start with the rendered configuration:\n%s\n--- configuration ---\n%s", logs, perimeterProxyConfig(e, echo, 8080, website, e.AuthMode == gentianov1alpha1.AuthModeApp, limits))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return rig
}

// raw sends exactly these bytes and returns the status, the response's
// header block and its body. A client library would tidy the path first,
// which is the one thing these tests must not have done for them.
func (r *perimeterRig) raw(request string) (int, string, string) {
	conn, err := net.DialTimeout("tcp", r.addr, 5*time.Second)
	if err != nil {
		return 0, "", ""
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		return 0, "", ""
	}
	all, _ := io.ReadAll(bufio.NewReader(conn))
	head, body, _ := bytes.Cut(all, []byte("\r\n\r\n"))
	fields := strings.Fields(strings.SplitN(string(head), "\r\n", 2)[0])
	if len(fields) < 2 {
		return 0, string(head), string(body)
	}
	status, _ := strconv.Atoi(fields[1])
	return status, string(head), string(body)
}

func (r *perimeterRig) get(path string, headers ...string) (int, string, string) {
	return r.request("GET", path, headers...)
}

func (r *perimeterRig) request(method, path string, headers ...string) (int, string, string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: share.example.org\r\nConnection: close\r\n", method, path)
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	return r.raw(b.String())
}

func reached(body string) bool { return strings.HasPrefix(body, "REACHED") }

func generousLimits() perimeterLimits {
	return perimeterLimits{MaxBody: "10m", RatePerSecond: 10000, Burst: 10000, Concurrent: 1000}
}

func sharesEntry() *gentianov1alpha1.ExposureSpec {
	return &gentianov1alpha1.ExposureSpec{
		Name: "shares", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		Paths:     []string{"/s/", "/public.php", "/remote.php/dav/"},
		DenyPaths: []string{"/remote.php/dav/systemtags/"},
		Backend:   gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	}
}

// A declared prefix cannot be left and a denied path cannot be reached,
// however the path is written.
func TestTheProxyItselfKeepsToTheDeclaredPaths(t *testing.T) {
	rig := startPerimeterRig(t, sharesEntry(), false, generousLimits())

	for _, path := range []string{
		"/s/abc",
		"/s/abc?next=../../admin&x=%2f..%2f", // a query is the application's to read
		"/s/%61bc",
		"/s/a%20b.txt",
		"/public.php",
		"/public.php/webdav",
		"/public.php?service=files",
		"/remote.php/dav/files/alice/report.pdf",
		"/remote.php/dav/systemtag", // not the denied prefix
	} {
		status, _, body := rig.get(path)
		if status != 200 || !reached(body) {
			t.Errorf("a declared path did not reach the application: %s -> %d", path, status)
		}
		if reached(body) && !strings.Contains(body, "uri="+path+"\n") {
			t.Errorf("the application was not sent the path as written: %s\n%s", path, body)
		}
	}

	for _, path := range []string{
		// Not declared.
		"/", "/admin", "/index.php/login", "/sx", "/s", "/public.phpx", "/public.php.bak", "/remote.php/davx",
		"/remote.php/webdav", "/S/abc", "/Public.php",
		// Leaving a declared prefix.
		"/s/../admin", "/s/../../admin", "/s/x/../../admin", "/s/..", "/s/x/..", "/s/.", "/s/./x",
		"/s/%2e%2e/admin", "/s/%2E%2E/admin", "/s/.%2e/admin", "/s/%2e./admin", "/s/%2e/x",
		"/s/..%2fadmin", "/s/..%2Fadmin", "/s/%2e%2e%2fadmin", "/s%2f..%2fadmin", "/s/x%2f..%2f..%2fadmin",
		"/s/..%5cadmin", "/s/..\\admin", "/s\\..\\admin", "/s/%5c..%5cadmin",
		"/s/..;/admin", "/s/;/../admin", "/s/x;a=b/../../admin", "/s/..%3b/admin",
		"//s/abc", "/s//abc", "/s/abc//", "//admin", "/s/..//admin",
		"/s/..%252fadmin/../../x", "/s/%00", "/s/x%00.txt",
		"/public.php/../index.php", "/public.php/..%2findex.php",
		"http://other.example.org/admin", "http://other.example.org/s/../admin",
		// Reaching a denied path.
		"/remote.php/dav/systemtags/", "/remote.php/dav/systemtags/1", "/remote.php/dav/SystemTags/1",
		"/remote.php/dav/SYSTEMTAGS/", "/remote.php/dav/%73ystemtags/1", "/remote.php/dav/system%74ags/1",
		"/remote.php/dav//systemtags/1", "/remote.php/dav/./systemtags/1", "/remote.php/dav/x/../systemtags/1",
		"/remote.php/dav/systemtags%2f1", "/remote.php/dav%2fsystemtags/1", "/remote.php/dav/systemtags;x/1",
		"/remote.php/dav/files/..%2fsystemtags/1", "/remote.php/dav/files/%2e%2e/systemtags/1",
		"/remote.php/dav\\systemtags/1",
	} {
		status, _, body := rig.get(path)
		if reached(body) || status == 200 || status == 0 {
			t.Errorf("reached the application, or no answer: %q -> %d\n%s", path, status, body)
		}
	}

	// A double-encoded dot segment is one segment with a percent sign in
	// its name to the proxy, and the application is sent it unchanged: it
	// is inside the prefix by both readings unless the application decodes
	// twice, which is then the application's defect and not a way round
	// the proxy's own decision.
	if status, _, body := rig.get("/s/%252e%252e/admin"); status != 200 || !strings.Contains(body, "uri=/s/%252e%252e/admin\n") {
		t.Errorf("a double-encoded segment was not passed on as written: %d\n%s", status, body)
	}

	for _, method := range []string{"TRACE", "TRACK", "CONNECT"} {
		status, _, body := rig.request(method, "/s/abc")
		if reached(body) || status < 400 {
			t.Errorf("%s reached the application: %d", method, status)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "OPTIONS", "PROPFIND", "REPORT", "MKCOL"} {
		status, _, body := rig.request(method, "/remote.php/dav/files/a", "Content-Length: 0")
		if status != 200 || !strings.Contains(body, "method="+method+"\n") {
			t.Errorf("%s did not reach the application: %d", method, status)
		}
	}
}

// Nothing a caller says of who they are or where they come from reaches the
// application, and nothing of the application's session comes back.
func TestTheProxyItselfRemovesWhatACallerClaims(t *testing.T) {
	rig := startPerimeterRig(t, sharesEntry(), false, generousLimits())

	claims := []string{
		"Cookie: zone=stolen",
		"Authorization: Bearer stolen",
		"X-Forwarded-Access-Token: stolen",
		"Forwarded: for=10.0.0.1;host=admin.internal",
		"X-Forwarded-Port: 8443",
		"X-Forwarded-Prefix: /admin",
		"X-Forwarded-Server: internal",
		"X-Original-URL: /admin",
		"X-Rewrite-URL: /admin",
		"X-Forwarded-Host: admin.internal",
		"X-Forwarded-Proto: http",
		"X-Real-IP: 10.0.0.1",
		// What the Gateway would have left: the caller's own claim, then
		// the address the Gateway saw the connection come from.
		"X-Forwarded-For: 10.0.0.1, 203.0.113.9",
	}
	for _, h := range perimeterStrippedIdentityHeaders() {
		claims = append(claims, h+": mallory")
		// The same name spelt the way some applications read it.
		claims = append(claims, strings.ReplaceAll(h, "-", "_")+": mallory")
		claims = append(claims, strings.ToLower(h)+": mallory")
	}
	status, head, body := rig.get("/s/abc", claims...)
	if status != 200 || !reached(body) {
		t.Fatalf("the request did not reach the application: %d\n%s", status, body)
	}
	if strings.Contains(body, "mallory") || strings.Contains(body, "stolen") || strings.Contains(body, "internal") ||
		strings.Contains(body, "/admin") || strings.Contains(body, "10.0.0.1") {
		t.Errorf("something the caller claimed reached the application:\n%s", body)
	}
	for _, want := range []string{
		"x-forwarded-for=[203.0.113.9]", "x-real-ip=[203.0.113.9]", "x-forwarded-proto=[https]",
		"x-forwarded-host=[share.example.org]", "cookie=[]", "authorization=[]", "forwarded=[]",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("the application was not told %s:\n%s", want, body)
		}
	}
	for _, h := range bouncer.IdentityHeaders() {
		if !strings.Contains(body, h+"=[]\n") {
			t.Errorf("the front door's %s was not removed:\n%s", h, body)
		}
	}

	lower := strings.ToLower(head)
	if strings.Contains(lower, "set-cookie") || strings.Contains(lower, "x-powered-by") || strings.Contains(lower, "the-application") {
		t.Errorf("the application's cookie or what it runs on came back out:\n%s", head)
	}
	if !strings.Contains(lower, "\r\nserver: nginx\r\n") {
		t.Errorf("the answer names more than the proxy:\n%s", head)
	}
}

func davEntry() *gentianov1alpha1.ExposureSpec {
	return &gentianov1alpha1.ExposureSpec{
		Name: "dav", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeApp,
		Paths:     []string{"/remote.php/dav/", "/ocs/"},
		DenyPaths: []string{"/remote.php/dav/systemtags/"},
		Backend:   gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	}
}

// An entry declared and approved to pass its callers' credential: the
// Authorization header reaches the application as the caller sent it, on the
// declared paths and on no other, and that is all that changes. A cookie
// still does not go in, nothing a caller says of who they are goes in, and
// the application's own cookie does not come out.
func TestTheProxyItselfPassesTheCallersCredentialAndNothingElse(t *testing.T) {
	limits := generousLimits()
	limits.CredentialRatePerSecond, limits.CredentialBurst, limits.CredentialConcurrent = 10000, 10000, 1000
	rig := startPerimeterRig(t, davEntry(), false, limits)

	claims := []string{
		"Cookie: zone=stolen; nc_session=stolen",
		"X-Forwarded-Access-Token: stolen",
		"Forwarded: for=10.0.0.1;host=admin.internal",
		"X-Original-URL: /admin",
		"X-Forwarded-Host: admin.internal",
		"X-Real-IP: 10.0.0.1",
		"X-Forwarded-For: 10.0.0.1, 203.0.113.9",
	}
	for _, h := range perimeterStrippedIdentityHeaders() {
		claims = append(claims, h+": mallory", strings.ReplaceAll(h, "-", "_")+": mallory", strings.ToLower(h)+": mallory")
	}
	for _, credential := range []string{
		"Basic YWxpY2U6YXBwLXBhc3N3b3Jk",
		"Bearer syt_an_apps_own_token",
		"Token abc123",
	} {
		status, head, body := rig.get("/remote.php/dav/files/alice/", append([]string{"Authorization: " + credential}, claims...)...)
		if status != 200 || !reached(body) {
			t.Fatalf("the request did not reach the application: %d\n%s", status, body)
		}
		if !strings.Contains(body, "authorization=["+credential+"]\n") {
			t.Errorf("the caller's credential did not reach the application as sent:\n%s", body)
		}
		if strings.Contains(body, "mallory") || strings.Contains(body, "stolen") || strings.Contains(body, "internal") ||
			strings.Contains(body, "/admin") || strings.Contains(body, "10.0.0.1") {
			t.Errorf("something else the caller claimed reached the application:\n%s", body)
		}
		for _, want := range []string{"cookie=[]", "x-forwarded-access-token=[]", "x-forwarded-for=[203.0.113.9]", "x-real-ip=[203.0.113.9]", "x-forwarded-proto=[https]"} {
			if !strings.Contains(body, want+"\n") {
				t.Errorf("the application was not told %s:\n%s", want, body)
			}
		}
		for _, h := range bouncer.IdentityHeaders() {
			if !strings.Contains(body, h+"=[]\n") {
				t.Errorf("the front door's %s was not removed:\n%s", h, body)
			}
		}
		if lower := strings.ToLower(head); strings.Contains(lower, "set-cookie") {
			t.Errorf("the application's cookie came back out:\n%s", head)
		}
	}
	// A request with no credential is the application's to refuse, not the
	// proxy's: it is passed on with none.
	if status, _, body := rig.get("/ocs/v2.php/cloud/capabilities"); status != 200 || !strings.Contains(body, "authorization=[]\n") {
		t.Errorf("a request without a credential: %d\n%s", status, body)
	}
	// Outside the declared paths a credential opens nothing: the request
	// does not reach the application at all.
	for _, path := range []string{
		"/", "/index.php/login", "/remote.php/webdav", "/ocsx", "/remote.php/dav/../../index.php",
		"/remote.php/dav/..%2f..%2findex.php", "/remote.php/dav/systemtags/1", "/remote.php/dav/SystemTags/1",
	} {
		status, _, body := rig.get(path, "Authorization: Basic YWxpY2U6YXBwLXBhc3N3b3Jk")
		if reached(body) || status == 200 || status == 0 {
			t.Errorf("a credential reached the application outside the declared paths: %q -> %d", path, status)
		}
	}
}

// The same paths under an entry that does not pass the credential are as
// they always were: the header does not arrive.
func TestTheProxyItselfStillRemovesTheCredentialOfAnEntryThatDidNotAsk(t *testing.T) {
	plain := davEntry()
	plain.AuthMode = gentianov1alpha1.AuthModeNone
	rig := startPerimeterRig(t, plain, false, generousLimits())
	status, _, body := rig.get("/remote.php/dav/files/alice/", "Authorization: Basic YWxpY2U6YXBwLXBhc3N3b3Jk")
	if status != 200 || !strings.Contains(body, "authorization=[]\n") {
		t.Fatalf("an entry that passes nothing passed the credential: %d\n%s", status, body)
	}
}

// Such an entry is held to its own, lower rate, and answers 429 beyond it:
// a burst of guesses from one address is cut off where the same burst
// against an entry that passes nothing would not be.
func TestTheProxyItselfHoldsAnEntryThatPassesTheCredentialToItsOwnLimit(t *testing.T) {
	// The general limit is far away; only the credential limit can answer 429.
	limits := perimeterLimits{MaxBody: "10m", RatePerSecond: 10000, Burst: 10000, Concurrent: 1000,
		CredentialRatePerSecond: 1, CredentialBurst: 4, CredentialConcurrent: 100}
	rig := startPerimeterRig(t, davEntry(), false, limits)
	guess := func(address string, n int) (ok, limited int) {
		for i := 0; i < n; i++ {
			switch status, _, _ := rig.get("/remote.php/dav/files/alice/",
				"X-Forwarded-For: "+address, fmt.Sprintf("Authorization: Basic guess-%d", i)); status {
			case 200:
				ok++
			case 429:
				limited++
			default:
				t.Errorf("unexpected answer %d", status)
			}
		}
		return ok, limited
	}
	ok, limited := guess("203.0.113.1", 25)
	if limited == 0 || ok < 4 || ok > 8 {
		t.Errorf("25 guesses from one address with a burst of 4: %d passed, %d answered 429", ok, limited)
	}
	if ok, limited := guess("203.0.113.2", 3); ok != 3 {
		t.Errorf("a second address was held to the first one's limit: %d passed, %d answered 429", ok, limited)
	}
	// The platform's defaults, as rendered: the proxy starts with them.
	clearEdgeLimitEnv(t)
	defaults := startPerimeterRig(t, davEntry(), false, perimeterLimitsFromEnv(""))
	if status, _, body := defaults.get("/remote.php/dav/", "Authorization: Basic x"); status != 200 || !reached(body) {
		t.Errorf("the proxy with the default limits: %d", status)
	}
}

// A body, a header and a request line over the limits are refused by the
// proxy, and the application is not sent them.
func TestTheProxyItselfHoldsARequestToItsSizes(t *testing.T) {
	limits := generousLimits()
	limits.MaxBody = "1m"
	rig := startPerimeterRig(t, sharesEntry(), false, limits)

	if status, _, body := rig.request("POST", "/s/upload", "Content-Length: 1048577"); status != 413 || reached(body) {
		t.Errorf("a body over the limit: %d, want 413", status)
	}
	if status, _, body := rig.request("POST", "/s/upload", "Content-Length: 5", "", "hello"); status != 200 || !reached(body) {
		t.Errorf("a small body did not reach the application: %d", status)
	}
	if status, _, body := rig.get("/s/abc", "X-Big: "+strings.Repeat("a", 9000)); status < 400 || reached(body) {
		t.Errorf("a 9k header: %d, want a refusal", status)
	}
	if status, _, body := rig.get("/s/" + strings.Repeat("a", 9000)); status < 400 || reached(body) {
		t.Errorf("a 9k request line: %d, want a refusal", status)
	}
	var many []string
	for i := 0; i < 8; i++ {
		many = append(many, fmt.Sprintf("X-Big-%d: %s", i, strings.Repeat("a", 7000)))
	}
	if status, _, body := rig.get("/s/abc", many...); status < 400 || reached(body) {
		t.Errorf("56k of headers: %d, want a refusal", status)
	}
}

// One client address is held to its rate, and another is not held to the
// first one's.
func TestTheProxyItselfLimitsEachClientAddress(t *testing.T) {
	burst := func(rig *perimeterRig, header string, n int) (ok, limited int) {
		for i := 0; i < n; i++ {
			switch status, _, _ := rig.get("/s/abc", header); status {
			case 200:
				ok++
			case 429:
				limited++
			default:
				rig.t.Errorf("unexpected answer %d", status)
			}
		}
		return ok, limited
	}

	t.Run("the address the Gateway saw", func(t *testing.T) {
		rig := startPerimeterRig(t, sharesEntry(), false, perimeterLimits{MaxBody: "10m", RatePerSecond: 1, Burst: 4, Concurrent: 100})
		// The caller's own claim comes first and changes with every
		// request; the Gateway's word is last and is what counts.
		ok, limited := burst(rig, "X-Forwarded-For: 198.51.100.77, 203.0.113.1", 25)
		if limited == 0 || ok < 4 || ok > 8 {
			t.Errorf("one address, 25 requests at once with a burst of 4: %d passed, %d answered 429", ok, limited)
		}
		// An address that has asked nothing yet is answered.
		if ok, limited := burst(rig, "X-Forwarded-For: 203.0.113.2", 3); ok != 3 {
			t.Errorf("a second address was held to the first one's limit: %d passed, %d answered 429", ok, limited)
		}
		// And saying one is somebody else changes nothing.
		if ok, _ := burst(rig, "X-Forwarded-For: 203.0.113.250, 203.0.113.1", 5); ok > 2 {
			t.Errorf("the first address got past its limit by claiming another: %d passed", ok)
		}
		// A refused path does not count against anybody.
		for i := 0; i < 30; i++ {
			rig.get("/admin", "X-Forwarded-For: 203.0.113.3")
		}
		if ok, _ := burst(rig, "X-Forwarded-For: 203.0.113.3", 3); ok != 3 {
			t.Errorf("refused requests used up an address's allowance: %d of 3 passed", ok)
		}
	})

	t.Run("the address a tunnel names", func(t *testing.T) {
		limits := perimeterLimits{MaxBody: "10m", RatePerSecond: 1, Burst: 4, Concurrent: 100, ClientAddressHeader: "CF-Connecting-IP"}
		rig := startPerimeterRig(t, sharesEntry(), false, limits)
		// Every request arrives from the tunnel's one address.
		one := "X-Forwarded-For: 10.42.0.9\r\nCF-Connecting-IP: 203.0.113.1"
		two := "X-Forwarded-For: 10.42.0.9\r\nCF-Connecting-IP: 2001:db8::2"
		ok, limited := burst(rig, one, 25)
		if limited == 0 || ok < 4 || ok > 8 {
			t.Errorf("one address behind the tunnel: %d passed, %d answered 429", ok, limited)
		}
		if ok, limited := burst(rig, two, 3); ok != 3 {
			t.Errorf("a second address behind the tunnel was held to the first one's limit: %d passed, %d answered 429", ok, limited)
		}
		status, _, body := rig.get("/s/abc", two)
		if status != 200 || !strings.Contains(body, "x-forwarded-for=[2001:db8::2]\n") {
			t.Errorf("the application was not told the address the tunnel named: %d\n%s", status, body)
		}
	})
}

// A path that would be configuration is not rendered, and an entry with a
// denied path that cannot be rendered publishes nothing. The proxy starts
// with the result either way.
func TestTheProxyItselfStartsWhateverAProfileDeclares(t *testing.T) {
	hostile := &gentianov1alpha1.ExposureSpec{
		Name: "hostile", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		Paths: []string{
			"/ok/", "/a b", "/x { } location /pwn { return 200 pwned; } #", "/q\"uote", "/semi;colon", "/dollar$var",
			"/new\nline", "/brace{", "/back\\slash", "/re(gex|p)+.*", "/dotted.path/",
		},
		Backend: gentianov1alpha1.BackendRef{Service: "app", Port: 8080},
	}
	rig := startPerimeterRig(t, hostile, false, generousLimits())
	for path, want := range map[string]bool{
		"/ok/x": true, "/dotted.path/x": true, "/dottedXpath/x": false, "/pwn": false, "/a": false, "/re": false, "/regex": false, "/": false,
	} {
		status, _, body := rig.get(path)
		if reached(body) != want || strings.Contains(body, "pwned") {
			t.Errorf("%s: reached=%v status=%d, want reached=%v", path, reached(body), status, want)
		}
	}

	hostile.DenyPaths = []string{"/ok/private/", "/ok/x y"}
	closed := startPerimeterRig(t, hostile, false, generousLimits())
	if status, _, body := closed.get("/ok/x"); reached(body) {
		t.Errorf("an entry with a denied path that cannot be rendered published something: %d", status)
	}
}

// On the cluster's main address the platform's paths are refused in any
// spelling, and every answer says nosniff.
func TestTheProxyItselfKeepsThePlatformsPathsOnTheMainAddress(t *testing.T) {
	site := &gentianov1alpha1.ExposureSpec{
		Name: "site", Surface: gentianov1alpha1.SurfacePerimeter, AuthMode: gentianov1alpha1.AuthModeNone,
		Apex: true, Paths: []string{"/"},
		Backend: gentianov1alpha1.BackendRef{Service: "website", Port: 8080},
	}
	rig := startPerimeterRig(t, site, true, generousLimits())
	status, head, body := rig.get("/about")
	if status != 200 || !reached(body) || !strings.Contains(strings.ToLower(head), "x-content-type-options: nosniff") {
		t.Errorf("the website's own page: %d\n%s", status, head)
	}
	for _, path := range []string{
		"/sign-in", "/Sign-In", "/branding/logo.svg", "/BRANDING/logo.svg", "/.well-known/acme-challenge/token",
		"/.well-known/pki-validation/x", "/.well-known/ACME-challenge/token", "/%2ewell-known/acme-challenge/token",
		"/x/../branding/logo.svg", "//branding/logo.svg", "/branding%2flogo.svg",
	} {
		if status, _, body := rig.get(path); reached(body) || status == 200 {
			t.Errorf("the website answered a path that is the platform's: %s -> %d", path, status)
		}
	}
	if status, _, body := rig.get("/.well-known/security.txt"); status != 200 || !reached(body) {
		t.Errorf("the rest of /.well-known/ is the website's: %d", status)
	}
}
