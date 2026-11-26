// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPom = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <parent><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-parent</artifactId><version>3.5.0</version></parent>
  <dependencies>
    <dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-web</artifactId></dependency>
    <dependency><groupId>org.xerial</groupId><artifactId>sqlite-jdbc</artifactId></dependency>
    <dependency><groupId>com.example</groupId><artifactId>reports</artifactId></dependency>
    <dependency><groupId>org.junit.jupiter</groupId><artifactId>junit-jupiter</artifactId><scope>test</scope></dependency>
  </dependencies>
</project>`

func TestJavaRuntimeDetection(t *testing.T) {
	rt, _, ok := detectRuntime(func(rel string) bool { return rel == "pom.xml" || rel == "index.html" })
	assert.True(t, ok)
	assert.Equal(t, RuntimeJava, rt, "a Spring Boot app with an index.html page is still a Java app")
	rt, _, _ = detectRuntime(func(rel string) bool { return rel == "pom.xml" || rel == "main.py" })
	assert.Equal(t, RuntimePython, rt)

	release := t.TempDir()
	require.NoError(t, writeReleaseRuntime(release, RuntimeJava, "target/app-0.1.jar"))
	rt, jar := releaseRuntime(release)
	assert.Equal(t, RuntimeJava, rt)
	assert.Equal(t, "target/app-0.1.jar", jar)
}

func TestDeniedJavaDependencies(t *testing.T) {
	pom, err := parsePom([]byte(testPom))
	require.NoError(t, err)
	assert.Equal(t, "3.5.0", pom.Parent.Version)
	// Spring Boot's own starters and test-only libraries need no approval.
	assert.Equal(t, []string{"org.xerial:sqlite-jdbc", "com.example:reports"}, deniedJavaDependencies(pom, nil))
	assert.Equal(t, []string{"com.example:reports"}, deniedJavaDependencies(pom, []string{"org.xerial:sqlite-jdbc"}))

	_, err = parsePom([]byte("<project><dependencies>"))
	assert.Error(t, err)
}

func TestInjectJavaSocketShim(t *testing.T) {
	dir := t.TempDir()
	imports := filepath.Join(dir, filepath.FromSlash(javaImportsPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(imports), 0o700))
	require.NoError(t, os.WriteFile(imports, []byte("com.example.OwnConfig"), 0o600))

	require.NoError(t, injectJavaSocketShim(dir))
	require.NoError(t, injectJavaSocketShim(dir), "a second build of the same tree adds nothing")

	body, err := os.ReadFile(imports)
	require.NoError(t, err)
	assert.Equal(t, "com.example.OwnConfig\n"+javaShimClass+"\n", string(body), "the app's own auto-configuration is kept")
	shim, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(javaShimPath)))
	require.NoError(t, err)
	assert.Contains(t, string(shim), "unixDomainSocketPath")
}

func TestFindBootJar(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.MkdirAll(target, 0o700))
	writeJar := func(name, manifest string) {
		f, err := os.Create(filepath.Join(target, name))
		require.NoError(t, err)
		w := zip.NewWriter(f)
		m, err := w.Create("META-INF/MANIFEST.MF")
		require.NoError(t, err)
		_, _ = m.Write([]byte(manifest))
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())
	}
	_, err := findBootJar(dir)
	assert.Error(t, err, "nothing built")

	writeJar("app-0.1-plain.jar", "Manifest-Version: 1.0\n")
	writeJar("app-0.1-sources.jar", "Manifest-Version: 1.0\nSpring-Boot-Version: 3.5.0\n")
	_, err = findBootJar(dir)
	assert.Error(t, err, "a plain jar cannot run on its own")

	writeJar("app-0.1.jar", "Manifest-Version: 1.0\nMain-Class: org.springframework.boot.loader.launch.JarLauncher\nSpring-Boot-Version: 3.5.0\n")
	jar, err := findBootJar(dir)
	require.NoError(t, err)
	assert.Equal(t, "target/app-0.1.jar", jar)
}
