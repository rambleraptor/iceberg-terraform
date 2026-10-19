// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command testcatalog runs the Apache Iceberg REST fixture for the provider's
// acceptance tests.
//
//	go run ./dev/testcatalog run -- go test ./...   # start, run the command, stop
//	go run ./dev/testcatalog serve                  # start and wait for Ctrl-C
//
// The fixture jar is taken from the apache/iceberg-rest-fixture image and run
// with the java on PATH (Java 17 or later), so the same setup works on Linux,
// macOS and Windows without Docker.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"time"
)

const catalogURI = "http://localhost:8181"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		args := os.Args[2:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			usage()
		}
		os.Exit(run(args))
	case "serve":
		os.Exit(serve())
	}
	usage()
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: testcatalog run -- <command> [args...]")
	fmt.Fprintln(os.Stderr, "       testcatalog serve")
	os.Exit(2)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "testcatalog: "+format+"\n", args...)
}

// startCatalog starts the fixture and waits for it to answer, printing its
// log and stopping it if it doesn't.
func startCatalog() (*catalog, error) {
	c, err := start()
	if err != nil {
		return nil, err
	}
	if err := waitReady(c.exited, 2*time.Minute); err != nil {
		c.printLog()
		_ = c.stop()

		return nil, err
	}

	return c, nil
}

func waitReady(exited <-chan struct{}, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, catalogURI+"/v1/config", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return errors.New("catalog exited before it became ready")
		case <-ctx.Done():
			return fmt.Errorf("catalog not ready at %s after %s", catalogURI, timeout)
		case <-time.After(time.Second):
		}
	}
}

func run(args []string) int {
	c, err := startCatalog()
	if err != nil {
		logf("%v", err)

		return 1
	}
	defer func() {
		if err := c.stop(); err != nil {
			logf("stop: %v", err)
		}
	}()

	// Ctrl-C reaches the child directly; keep running so the catalog is
	// stopped once the child exits.
	signal.Ignore(os.Interrupt)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "TF_ACC=1", "ICEBERG_CATALOG_URI="+catalogURI)
	err = cmd.Run()
	if err == nil {
		return 0
	}
	c.printLog()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	logf("%v", err)

	return 1
}

func serve() int {
	c, err := startCatalog()
	if err != nil {
		logf("%v", err)

		return 1
	}
	logf("Iceberg REST catalog ready at %s (Ctrl-C to stop)", catalogURI)
	logf("run tests with TF_ACC=1 ICEBERG_CATALOG_URI=%s go test ./...", catalogURI)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	code := 0
	select {
	case <-interrupt:
	case <-c.exited:
		logf("catalog exited")
		c.printLog()
		code = 1
	}
	if err := c.stop(); err != nil {
		logf("stop: %v", err)

		return 1
	}

	return code
}
