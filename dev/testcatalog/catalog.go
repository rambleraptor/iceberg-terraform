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

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// catalog is a running REST fixture. Its catalog lives in a SQLite file in a
// temporary directory and table files are kept in memory, so nothing depends
// on S3 or on Hadoop's local filesystem (which needs winutils.exe on Windows).
type catalog struct {
	dir    string
	cmd    *exec.Cmd
	exited chan struct{}
}

func start() (*catalog, error) {
	jar, err := fixtureJar()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "testcatalog-")
	if err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(dir, "catalog.log"))
	if err != nil {
		os.RemoveAll(dir)

		return nil, err
	}

	cmd := exec.Command("java", "-jar", jar)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(),
		// The fixture's default, jdbc:sqlite::memory:, gives every pooled
		// connection its own empty database.
		"CATALOG_URI=jdbc:sqlite:"+filepath.ToSlash(filepath.Join(dir, "catalog.db")),
		"CATALOG_WAREHOUSE=memory://warehouse",
		"CATALOG_IO__IMPL=org.apache.iceberg.inmemory.InMemoryFileIO",
	)
	if err := cmd.Start(); err != nil {
		log.Close()
		os.RemoveAll(dir)

		return nil, fmt.Errorf("starting java (is Java 17+ on PATH?): %w", err)
	}

	c := &catalog{dir: dir, cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		log.Close()
		close(c.exited)
	}()

	return c, nil
}

func (c *catalog) stop() error {
	_ = c.cmd.Process.Kill()
	<-c.exited

	return os.RemoveAll(c.dir)
}

func (c *catalog) printLog() {
	f, err := os.Open(filepath.Join(c.dir, "catalog.log"))
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(os.Stderr, "testcatalog: ---- catalog log ----")
	_, _ = io.Copy(os.Stderr, f)
}
