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
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// The REST fixture is only published as a Docker image, so the jar is read
// straight out of the image's layers over the registry API.
const (
	fixtureRepo = "apache/iceberg-rest-fixture"
	// linux/amd64 manifest of apache/iceberg-rest-fixture:1.10.1. The jar is
	// the same for every platform.
	fixtureManifest  = "sha256:319afc30ef94300f79987b1d185c4911576db0cdfaa98246cb42757ec4d53463"
	fixtureJarPath   = "usr/lib/iceberg-rest/iceberg-rest-adapter.jar"
	fixtureJarSHA256 = "49d01ec8e0995001a92b4c880adfe184d602541ba39d135f105511101da4db7d"
)

var (
	registryURL = "https://registry-1.docker.io"
	authURL     = "https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + fixtureRepo + ":pull"
)

// fixtureJar returns the path of the cached fixture jar, downloading it
// first if needed.
func fixtureJar() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "iceberg-terraform-testcatalog")
	path := filepath.Join(dir, "iceberg-rest-adapter-"+fixtureJarSHA256[:12]+".jar")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	logf("downloading REST fixture jar from %s@%s", fixtureRepo, fixtureManifest)
	if err := downloadJar(path); err != nil {
		return "", fmt.Errorf("downloading fixture jar: %w", err)
	}

	return path, nil
}

func downloadJar(dst string) error {
	var tok struct {
		Token string `json:"token"`
	}
	if err := getJSON(authURL, "", "", "", &tok); err != nil {
		return err
	}

	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := getJSON(registryURL+"/v2/"+fixtureRepo+"/manifests/"+fixtureManifest, tok.Token,
		"application/vnd.oci.image.manifest.v1+json", fixtureManifest, &manifest); err != nil {
		return err
	}

	// The jar is copied in by one of the last layers.
	for i := len(manifest.Layers) - 1; i >= 0; i-- {
		l := manifest.Layers[i]
		if l.MediaType != "application/vnd.oci.image.layer.v1.tar+gzip" &&
			l.MediaType != "application/vnd.docker.image.rootfs.diff.tar.gzip" {
			return fmt.Errorf("layer %s: unsupported media type %s", l.Digest, l.MediaType)
		}
		found, err := extractFromLayer(registryURL+"/v2/"+fixtureRepo+"/blobs/"+l.Digest, tok.Token, l.Digest, dst)
		if err != nil {
			return fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		if found {
			return nil
		}
	}

	return fmt.Errorf("%s not found in image", fixtureJarPath)
}

func get(url, token, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()

		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	return resp, nil
}

// getJSON decodes url into v, first checking the body against digest if it
// is set.
func getJSON(url, token, accept, digest string, v any) error {
	resp, err := get(url, token, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if digest != "" {
		if err := checkDigest(body, digest); err != nil {
			return err
		}
	}

	return json.Unmarshal(body, v)
}

func checkDigest(b []byte, digest string) error {
	sum := sha256.Sum256(b)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("digest mismatch: got %s, want %s", got, digest)
	}

	return nil
}

// extractFromLayer streams a gzipped layer, writing fixtureJarPath to dst if
// the layer contains it. The whole layer is read so its digest can be
// checked before dst is put in place.
func extractFromLayer(url, token, digest, dst string) (bool, error) {
	resp, err := get(url, token, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	layerHash := sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(resp.Body, layerHash))
	if err != nil {
		return false, err
	}

	tmp := dst + ".tmp"
	defer os.Remove(tmp)
	found := false
	jarHash := sha256.New()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		if hdr.Name != fixtureJarPath && hdr.Name != "./"+fixtureJarPath {
			continue
		}
		f, err := os.Create(tmp)
		if err != nil {
			return false, err
		}
		_, err = io.Copy(io.MultiWriter(f, jarHash), tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return false, err
		}
		found = true
	}
	// Drain anything after the tar end-of-archive marker so the digest
	// covers the whole blob.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return false, err
	}
	if _, err := io.Copy(layerHash, resp.Body); err != nil {
		return false, err
	}
	if got := "sha256:" + hex.EncodeToString(layerHash.Sum(nil)); got != digest {
		return false, fmt.Errorf("digest mismatch: got %s", got)
	}
	if !found {
		return false, nil
	}
	if got := hex.EncodeToString(jarHash.Sum(nil)); got != fixtureJarSHA256 {
		return false, fmt.Errorf("jar sha256 mismatch: got %s, want %s", got, fixtureJarSHA256)
	}

	return true, os.Rename(tmp, dst)
}
