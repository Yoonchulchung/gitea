// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "embed"
)

// Spring Boot apps: a pom.xml at the top is built with Maven inside the
// sandbox, and the jar it makes is run with the server's Java. Tomcat
// listens on the app's unix socket through a configuration class the build
// adds, so the app needs no port and no code of its own for it.
// See docs/company/runtimes.md.

const (
	javaMemoryMBDefault = 512
	// Every JVM thread counts as a process: Tomcat's workers alone are 50.
	javaProcessesDefault = 256
	javaBuildTimeout     = 15 * time.Minute
	javaBuildMemoryMB    = 2048
)

//go:embed appshim/PlatformSocketConfiguration.java
var javaSocketShim []byte

const (
	javaShimPath    = "src/main/java/company/platform/PlatformSocketConfiguration.java"
	javaShimClass   = "company.platform.PlatformSocketConfiguration"
	javaImportsPath = "src/main/resources/META-INF/spring/org.springframework.boot.autoconfigure.AutoConfiguration.imports"
)

type javaInfo struct {
	Java, JavaVersion   string
	Maven, MavenVersion string // empty when Maven is missing: running a built app needs only Java
	MavenErr            error
}

var javaVersionPattern = regexp.MustCompile(`version "([^"]+)"`)

// javaProbe finds java and mvn, rechecked like pythonProbe.
var javaProbe = probeEvery(time.Minute, func() (javaInfo, error) {
	var info javaInfo
	java, err := lookupTool("JAVA_PATH", "java")
	if err != nil {
		return info, errors.New("java was not found — install a JDK (17 or later), or set [company] JAVA_PATH")
	}
	out, err := exec.Command(java, "-version").CombinedOutput() //nolint:gosec // from PATH or an admin-set key
	if err != nil {
		return info, fmt.Errorf("%s could not be run: %w", java, err)
	}
	if m := javaVersionPattern.FindSubmatch(out); m != nil {
		info.JavaVersion = string(m[1])
	}
	info.Java = java
	mvn, err := lookupTool("MAVEN_PATH", "mvn")
	if err != nil {
		info.MavenErr = errors.New("mvn was not found — install Maven, or set [company] MAVEN_PATH")
		return info, nil
	}
	out, err = exec.Command(mvn, "-v").CombinedOutput() //nolint:gosec // from PATH or an admin-set key
	if err != nil {
		info.MavenErr = fmt.Errorf("%s could not be run: %w", mvn, err)
		return info, nil
	}
	if first, _, _ := strings.Cut(string(out), "\n"); strings.HasPrefix(first, "Apache Maven ") {
		info.MavenVersion, _, _ = strings.Cut(strings.TrimPrefix(first, "Apache Maven "), " ")
	}
	info.Maven = mvn
	return info, nil
})

var errNoJava = audienceKeyError("company.err.no_java", "company.err.no_java.admin")

// Where libraries come from, the same rule as pip's: an internal repository,
// or public Maven Central only when an administrator said so.
func mavenRepositoryURL() string { return strings.TrimSpace(companySetting("MAVEN_REPOSITORY_URL")) }

func mavenEgressAllowed() bool {
	return mavenRepositoryURL() != "" || companySetting("MAVEN_ALLOW_PUBLIC_REPOSITORY") == "true"
}

var errMavenNoRepo = audienceKeyError("company.err.maven_no_repo", "company.err.maven_no_repo.admin")

// javaHome is the JDK directory a java executable belongs to.
func javaHome(java string) string {
	if resolved, err := filepath.EvalSymlinks(java); err == nil {
		java = resolved
	}
	return filepath.Dir(filepath.Dir(java))
}

// mavenHome is the Maven directory an mvn executable belongs to.
func mavenHome(mvn string) string {
	if resolved, err := filepath.EvalSymlinks(mvn); err == nil {
		mvn = resolved
	}
	return filepath.Dir(filepath.Dir(mvn))
}

// pomDependency is one <dependency> a pom.xml names directly.
type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Scope      string `xml:"scope"`
}

func (d pomDependency) Name() string { return d.GroupID + ":" + d.ArtifactID }

type pomProject struct {
	Parent struct {
		GroupID string `xml:"groupId"`
		Version string `xml:"version"`
	} `xml:"parent"`
	Dependencies []pomDependency `xml:"dependencies>dependency"`
}

func parsePom(body []byte) (pomProject, error) {
	var p pomProject
	if err := xml.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("pom.xml could not be read: %w", err)
	}
	return p, nil
}

// javaBaseGroup is the platform's base stack for Java, as fastapi is for
// Python: Spring Boot's own starters need no approval.
const javaBaseGroup = "org.springframework.boot"

// deniedJavaDependencies is what a pom.xml asks for directly that is not
// approved. What those pull in is not asked about: approving Spring Boot's
// hundred transitive libraries one by one would approve nothing in practice.
func deniedJavaDependencies(p pomProject, allowed []string) []string {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[normalizePackageName(a)] = true
	}
	var denied []string
	for _, d := range p.Dependencies {
		if d.Scope == "test" || d.GroupID == javaBaseGroup || d.GroupID == "" {
			continue
		}
		if !ok[normalizePackageName(d.Name())] && !slices.Contains(denied, d.Name()) {
			denied = append(denied, d.Name())
		}
	}
	return denied
}

// injectJavaSocketShim adds the configuration class that puts Tomcat on the
// app's unix socket, compiled with the app's own Spring Boot.
func injectJavaSocketShim(appDir string) error {
	shim := filepath.Join(appDir, filepath.FromSlash(javaShimPath))
	if err := os.MkdirAll(filepath.Dir(shim), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(shim, javaSocketShim, 0o600); err != nil {
		return err
	}
	imports := filepath.Join(appDir, filepath.FromSlash(javaImportsPath))
	if err := os.MkdirAll(filepath.Dir(imports), 0o700); err != nil {
		return err
	}
	existing, err := os.ReadFile(imports)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(existing), javaShimClass) {
		return nil
	}
	body := strings.TrimRight(string(existing), "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(imports, []byte(body+javaShimClass+"\n"), 0o600)
}

// findBootJar picks the runnable jar Maven made: a Spring Boot one, not the
// plain, sources or javadoc jars beside it.
func findBootJar(appDir string) (string, error) {
	jars, _ := filepath.Glob(filepath.Join(appDir, "target", "*.jar"))
	for _, jar := range jars {
		base := filepath.Base(jar)
		if strings.HasSuffix(base, "-plain.jar") || strings.HasSuffix(base, "-sources.jar") || strings.HasSuffix(base, "-javadoc.jar") {
			continue
		}
		if isBootJar(jar) {
			rel, err := filepath.Rel(appDir, jar)
			return filepath.ToSlash(rel), err
		}
	}
	return "", errors.New("the build made no runnable Spring Boot jar — is spring-boot-maven-plugin in pom.xml?")
}

func isBootJar(path string) bool {
	r, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != "META-INF/MANIFEST.MF" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return false
		}
		defer rc.Close()
		buf := make([]byte, 64*1024)
		n, _ := rc.Read(buf)
		return strings.Contains(string(buf[:n]), "Spring-Boot-Version:")
	}
	return false
}

// buildJavaRelease compiles the app in appDir and records the jar to run.
func buildJavaRelease(ctx context.Context, p appPaths, release, appDir string, settings AppSettings, packagesFree bool) (string, error) {
	info, err := javaProbe()
	if err == nil {
		err = info.MavenErr
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoJava, err)
	}
	if !mavenEgressAllowed() {
		return "", errMavenNoRepo
	}
	body, err := os.ReadFile(filepath.Join(appDir, "pom.xml"))
	if err != nil {
		return "", err
	}
	pom, err := parsePom(body)
	if err != nil {
		return "", &packagesDeniedError{message: err.Error()}
	}
	if denied := deniedJavaDependencies(pom, settings.AllowedPackages()); len(denied) > 0 && !packagesFree {
		return "", &packagesDeniedError{message: "not approved yet: " + strings.Join(denied, ", "), packages: denied}
	}
	if err := injectJavaSocketShim(appDir); err != nil {
		return "", err
	}

	// Per app, so one app's build can never plant a library another app's
	// build then trusts.
	repo := filepath.Join(p.home, "maven-repository")
	tmp := filepath.Join(p.home, "build-tmp")
	for _, dir := range []string{repo, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	args := []string{info.Maven, "-B", "-q", "-e", "-DskipTests", "-Dmaven.repo.local=" + repo, "package"}
	if url := mavenRepositoryURL(); url != "" {
		settingsFile := filepath.Join(tmp, "settings.xml")
		mirror := fmt.Sprintf("<settings><mirrors><mirror><id>platform</id><mirrorOf>*</mirrorOf><url>%s</url></mirror></mirrors></settings>", xmlEscape(url))
		if err := os.WriteFile(settingsFile, []byte(mirror), 0o600); err != nil {
			return "", err
		}
		args = append(args[:len(args)-1], "-s", settingsFile, "package")
	}
	ctx, cancel := context.WithTimeout(ctx, javaBuildTimeout)
	defer cancel()
	cmd, err := buildToolCommand(ctx, appDir, []string{repo, tmp}, []string{javaHome(info.Java), mavenHome(info.Maven)}, args)
	if err != nil {
		return "", err
	}
	cmd.Env = []string{
		"PATH=" + filepath.Join(javaHome(info.Java), "bin") + ":/usr/local/bin:/usr/bin:/bin",
		"JAVA_HOME=" + javaHome(info.Java),
		"HOME=" + tmp,
		"LANG=C.UTF-8",
		"MAVEN_OPTS=-Xmx1g -Djava.io.tmpdir=" + tmp,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("the Maven build failed: %w\n%s", err, lastLines(string(out), 40))
	}
	jar, err := findBootJar(appDir)
	if err != nil {
		return "", err
	}
	return "built " + jar + " with Maven " + info.MavenVersion, writeReleaseRuntime(release, RuntimeJava, jar)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// javaStartArgs runs the jar with the heap sized to the app's memory limit,
// leaving room for what the JVM holds outside the heap.
func javaStartArgs(p appPaths, release, jar string, memoryMB int) ([]string, error) {
	info, err := javaProbe()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoJava, err)
	}
	_, tmp := appHomeAndTmp(p)
	return []string{
		info.Java,
		"-Xmx" + strconv.Itoa(max(memoryMB*60/100, 64)) + "m",
		"-Xss512k",
		"-XX:MaxMetaspaceSize=128m",
		"-XX:ReservedCodeCacheSize=64m",
		"-XX:+UseSerialGC",
		"-XX:TieredStopAtLevel=1", // starts faster and holds less; these are internal tools
		"-XX:-UsePerfData",        // no hsperfdata files in a shared /tmp
		"-Djava.io.tmpdir=" + tmp,
		"-Dfile.encoding=UTF-8",
		"-Dserver.forward-headers-strategy=framework", // the proxy's X-Forwarded-Prefix becomes the app's context
		"-Dserver.tomcat.threads.max=50",
		"-jar", filepath.Join(appCodeDirForProcess(filepath.Join(release, "app")), filepath.FromSlash(jar)),
	}, nil
}
