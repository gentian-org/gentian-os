/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// The run program is the first process of a run's pod, for the runtime that
// runs an image as a pod.
//
// An action's author writes a command that reads the run's inputs on standard
// input and writes its outputs to standard output. This program is what
// stands between that command and the component that started the run: it
// fetches the inputs from the component, runs the command with them, and
// delivers what the command wrote. The command inherits the environment, so
// one that wants more (progress, a file of evidence, a person's answer, a
// call to an adapter) asks the component itself, at RUN_CALLBACK_URL with
// RUN_TOKEN.
//
//	GET  $RUN_CALLBACK_URL/runs/$RUN_ID/inputs    the inputs, as the body
//	POST $RUN_CALLBACK_URL/runs/$RUN_ID/outputs   what the command wrote
//
// Both carry Authorization: Bearer $RUN_TOKEN, and the second says how the
// command ended (X-Run-Exit-Code) and which attempt it was (X-Run-Attempt).
//
// It is copied into the pod by an init container of the platform's image
// (run-program --install <path>), because the action's image does not hold
// it and must not have to.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxOutputs is the most a command may write as its outputs.
const maxOutputs = 16 << 20

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, env func(string) string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "run: "+format+"\n", a...)
		return 70
	}
	if len(args) == 2 && args[0] == "--install" {
		if err := install(args[1]); err != nil {
			return fail("%v", err)
		}
		return 0
	}
	if len(args) < 2 || args[0] != "--" {
		return fail("usage: run -- <command> [arguments], or run --install <path>")
	}
	base, token, id := strings.TrimRight(env("RUN_CALLBACK_URL"), "/"), env("RUN_TOKEN"), env("RUN_ID")
	if base == "" || token == "" || id == "" {
		return fail("RUN_CALLBACK_URL, RUN_TOKEN and RUN_ID are required")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	call := func(method, path string, body io.Reader, header map[string]string) ([]byte, error) {
		// The component may be rolling: asked a few times, a little apart.
		var payload []byte
		if body != nil {
			payload, _ = io.ReadAll(body)
		}
		var last error
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
			}
			req, err := http.NewRequestWithContext(context.Background(), method, base+"/runs/"+id+path, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			for k, v := range header {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				last = err
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOutputs))
			_ = resp.Body.Close()
			if err == nil && resp.StatusCode < 300 {
				return raw, nil
			}
			last = fmt.Errorf("%s %s: %s", method, path, resp.Status)
			if resp.StatusCode < 500 {
				break // the component refused; asking again changes nothing
			}
		}
		return nil, last
	}

	inputs, err := call(http.MethodGet, "/inputs", nil, nil)
	if err != nil {
		return fail("fetching the inputs: %v", err)
	}
	cmd := exec.Command(args[1], args[2:]...) //nolint:gosec // The action's declared command, from the dispatcher's table.
	cmd.Stdin, cmd.Stderr = bytes.NewReader(inputs), stderr
	outputs := &capped{limit: maxOutputs}
	cmd.Stdout = outputs
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fail("starting %s: %v", args[1], err)
		}
		code = exit.ExitCode()
	}
	if outputs.over {
		_, _ = fmt.Fprintf(stderr, "run: the command wrote more than %d bytes of outputs; the rest is dropped\n", maxOutputs)
		if code == 0 {
			code = 70
		}
	}
	if _, err := call(http.MethodPost, "/outputs", &outputs.buf, map[string]string{
		"Content-Type": "application/octet-stream", "X-Run-Exit-Code": strconv.Itoa(code), "X-Run-Attempt": env("RUN_ATTEMPT"),
	}); err != nil {
		return fail("delivering the outputs: %v", err)
	}
	return code
}

// capped keeps what is written to it up to a limit and drops the rest.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// install copies this program to path, for an init container to leave it
// where the run's own container finds it.
func install(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(self) //nolint:gosec // This program's own file.
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o555) //nolint:gosec // A program every user of the pod runs.
}
