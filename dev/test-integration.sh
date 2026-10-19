#!/usr/bin/env bash
#
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

# Runs the Apache Iceberg REST fixture for the provider's acceptance tests.
#
#   dev/test-integration.sh [command...]   start, run command (default: go test ./... -v), stop
#   dev/test-integration.sh serve          start and wait for Ctrl-C
#
# The fixture is only published as a Docker image, so its jar is extracted
# with crane and run with the java on PATH (Java 17 or later). The same setup
# works on Linux, macOS and Windows (Git Bash) without Docker.

set -euo pipefail

CRANE=github.com/google/go-containerregistry/cmd/crane@v0.22.1
# linux/amd64 manifest of apache/iceberg-rest-fixture:1.10.1. The jar is the
# same for every platform.
IMAGE=apache/iceberg-rest-fixture@sha256:319afc30ef94300f79987b1d185c4911576db0cdfaa98246cb42757ec4d53463
JAR_PATH=usr/lib/iceberg-rest/iceberg-rest-adapter.jar
JAR_SHA256=49d01ec8e0995001a92b4c880adfe184d602541ba39d135f105511101da4db7d
CATALOG_URI=http://localhost:8181

# Paths handed to java must be native on Windows.
native() {
  if command -v cygpath >/dev/null; then cygpath -m "$1"; else printf '%s\n' "$1"; fi
}

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

cache="${XDG_CACHE_HOME:-$HOME/.cache}/iceberg-terraform"
jar="$cache/iceberg-rest-adapter-${JAR_SHA256:0:12}.jar"
if [ ! -f "$jar" ]; then
  echo "Downloading REST fixture jar from $IMAGE" >&2
  mkdir -p "$cache"
  go run "$CRANE" export "$IMAGE" - | tar -xOf - "$JAR_PATH" > "$jar.tmp"
  if [ "$(sha256 "$jar.tmp")" != "$JAR_SHA256" ]; then
    echo "REST fixture jar checksum mismatch" >&2
    rm -f "$jar.tmp"
    exit 1
  fi
  mv "$jar.tmp" "$jar"
fi

dir=$(mktemp -d)
pid=
stop() {
  if [ -n "$pid" ]; then
    # Git Bash can't signal a native java.exe; kill it by its Windows PID.
    if [ -f "/proc/$pid/winpid" ]; then
      taskkill //F //T //PID "$(cat "/proc/$pid/winpid")" >/dev/null 2>&1 || true
    else
      kill "$pid" 2>/dev/null || true
    fi
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$dir"
}
trap stop EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The fixture's default catalog, jdbc:sqlite::memory:, gives every pooled
# connection its own empty database, so use a file. Table files are kept in
# memory, which avoids S3 and Hadoop's local filesystem (it needs winutils.exe
# on Windows).
CATALOG_URI="jdbc:sqlite:$(native "$dir/catalog.db")" \
CATALOG_WAREHOUSE=memory://warehouse \
CATALOG_IO__IMPL=org.apache.iceberg.inmemory.InMemoryFileIO \
  java -jar "$(native "$jar")" > "$dir/catalog.log" 2>&1 &
pid=$!

for _ in $(seq 120); do
  if curl -sf -o /dev/null "$CATALOG_URI/v1/config"; then
    break
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$dir/catalog.log" >&2
    echo "REST fixture exited before it became ready" >&2
    exit 1
  fi
  sleep 1
done
if ! curl -sf -o /dev/null "$CATALOG_URI/v1/config"; then
  cat "$dir/catalog.log" >&2
  echo "REST fixture not ready at $CATALOG_URI" >&2
  exit 1
fi

if [ "${1:-}" = serve ]; then
  echo "Iceberg REST catalog ready at $CATALOG_URI (Ctrl-C to stop)" >&2
  echo "Run tests with TF_ACC=1 ICEBERG_CATALOG_URI=$CATALOG_URI go test ./..." >&2
  wait "$pid"
  exit
fi

[ $# -gt 0 ] || set -- go test ./... -v
if ! TF_ACC=1 ICEBERG_CATALOG_URI=$CATALOG_URI "$@"; then
  echo "---- REST fixture log ----" >&2
  cat "$dir/catalog.log" >&2
  exit 1
fi
