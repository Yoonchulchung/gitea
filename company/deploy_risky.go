// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Capabilities an app gains by adding a line of Python, that a person should
// see before it ships. A fixed check rather than the AI's judgement, so that
// a model that is wrong — or talked into being wrong by the code it reads —
// cannot approve them on its own. It reads only added lines: an app that
// already had such code was approved with it by somebody.
var riskyPythonPatterns = []struct {
	label string
	why   string // what a person reviewing it should know, in a sentence
	re    *regexp.Regexp
}{
	{
		"명령 실행", "서버에서 운영체제 명령을 실행합니다. 요청 값이 섞이면 서버의 파일과 비밀값 전체에 손이 닿습니다.",
		regexp.MustCompile(`\bsubprocess\b|\bos\.(system|popen|exec\w*|spawn\w*)\b|\bfrom\s+os\s+import\b[^\n]*\b(system|popen|exec\w*|spawn\w*)\b|\bpty\b`),
	},
	{
		"동적 코드 실행", "문자열을 코드로 실행합니다. 외부 입력이 섞이면 어떤 코드든 실행될 수 있고, 코드만 읽어서는 무엇을 하는지 알 수 없습니다.",
		regexp.MustCompile(`(^|[^.\w])(exec|eval|compile)\s*\(|__builtins__|\bbuiltins\b`),
	},
	{
		"동적 import", "실행 중에 어떤 모듈을 불러올지 정합니다. 코드만 읽어서는 실제로 무엇이 실행되는지 알 수 없습니다.",
		regexp.MustCompile(`__import__\s*\(|\bimportlib\b`),
	},
	{
		"환경 변수 읽기", "환경 변수에는 API 키·DB 비밀번호 같은 비밀값이 들어 있습니다. 읽은 값이 화면에 보이거나, 로그에 남거나, 밖으로 보내지지 않는지 확인해야 합니다.",
		regexp.MustCompile(`\benviron\b|\bgetenv\b`),
	},
	{
		"외부 통신", "앱 밖의 서버에 연결합니다. 회사 데이터가 나가는 통로가 될 수 있으니, 어디로 무엇을 보내는지 확인해야 합니다.",
		regexp.MustCompile(`\burllib\b|\bhttp\.client\b|\brequests\b|\bhttpx\b|\baiohttp\b|\bsocket\b|\bftplib\b|\bsmtplib\b`),
	},
	{
		"인코딩된 데이터 해석", "감춰진 데이터를 풀어서 씁니다(base64·pickle 등). 겉으로 보이는 코드와 실제 동작이 다를 수 있습니다.",
		regexp.MustCompile(`\bb(64|32|16|85)decode\s*\(|\bpickle\.loads?\s*\(|\bmarshal\.loads?\s*\(|\bcodecs\.decode\s*\(`),
	},
}

// riskyFindingsPerFileMax keeps a file that does one thing everywhere from
// burying the rest of the table.
const riskyFindingsPerFileMax = 3

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// platformEnvNames are what the platform hands every app (company/appproc.go's
// buildEnv): reading DB_PATH is how an app is told to find its database, not a secret.
var platformEnvNames = map[string]bool{"SOCKET": true, "ROOT_PATH": true, "DATA_DIR": true, "DB_PATH": true, "BROKER_SOCKET": true, "HOME": true, "TMPDIR": true, "LANG": true}

var (
	envAccess     = regexp.MustCompile(`\benviron\b|\bgetenv\b`)
	envNamedRead  = regexp.MustCompile(`(?:\benviron\s*\[\s*|\benviron\.get\(\s*|\bgetenv\(\s*)['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)
	envImportOnly = regexp.MustCompile(`^\s*(?:from\s+os\s+import\s+[\w\s,]*|import\s+os(?:\s*,.*)?)$`)
)

// envReadIsKnown reports whether every environment access on the line reads
// one variable by name, and each name is the platform's or one the department
// set for this app. Whole-environment access, or a name built at run time,
// stays a finding: that is where secrets leave in bulk.
func envReadIsKnown(code string, allowed map[string]bool) bool {
	if envImportOnly.MatchString(code) {
		return true // the use, not the import, is what is judged
	}
	named := envNamedRead.FindAllStringSubmatch(code, -1)
	if len(named) == 0 || len(named) != len(envAccess.FindAllString(code, -1)) {
		return false
	}
	for _, m := range named {
		if !platformEnvNames[m[1]] && !allowed[m[1]] {
			return false
		}
	}
	return true
}

// riskyAdditions finds each capability the diff adds to a Python file: where,
// the line itself, and why a person should look at it. appEnv are the
// variable names the department set for this app (company/envstore.go).
func riskyAdditions(diff string, appEnv ...string) []aiFinding {
	allowed := map[string]bool{}
	for _, name := range appEnv {
		allowed[name] = true
	}
	type key struct{ label, file string }
	var out []aiFinding
	seen := map[key]int{}
	file, line := "", 0
	for raw := range strings.SplitSeq(diff, "\n") {
		if name, ok := strings.CutPrefix(raw, "+++ "); ok {
			file = shortPath(strings.TrimPrefix(strings.TrimSpace(name), "b/"))
			continue
		}
		if m := hunkHeader.FindStringSubmatch(raw); m != nil {
			line, _ = strconv.Atoi(m[1])
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+"):
		case strings.HasPrefix(raw, "-"):
			continue // a removed line does not move the new file's numbering
		default:
			line++ // context
			continue
		}
		current := line
		line++
		if !strings.HasSuffix(file, ".py") {
			continue
		}
		// Comments are read too: stripping them would let a "#" inside a
		// string hide the rest of the line, and a false alarm only means a person looks.
		code := raw[1:]
		for _, p := range riskyPythonPatterns {
			if !p.re.MatchString(code) {
				continue
			}
			if p.label == "환경 변수 읽기" && envReadIsKnown(code, allowed) {
				continue
			}
			k := key{p.label, file}
			seen[k]++
			if seen[k] > riskyFindingsPerFileMax {
				continue
			}
			out = append(out, aiFinding{
				File:  fmt.Sprintf("%s:%d", file, current),
				Issue: fmt.Sprintf("%s — %s 해당 코드: '%s'", p.label, p.why, truncateRunes(strings.TrimSpace(code), 120)),
			})
		}
	}
	return out
}

// riskyLabels names what was found, once each, for a one-line summary.
func riskyLabels(findings []aiFinding) string {
	var labels []string
	for _, p := range riskyPythonPatterns {
		for _, f := range findings {
			if strings.HasPrefix(f.Issue, p.label+" — ") {
				labels = append(labels, p.label)
				break
			}
		}
	}
	return strings.Join(labels, ", ")
}

// shortPath drops the department prefix (owner/name, deployPathPrefix) for display.
func shortPath(f string) string {
	if parts := strings.SplitN(f, "/", 3); len(parts) == 3 {
		return parts[2]
	}
	return f
}
