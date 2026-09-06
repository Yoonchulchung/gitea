// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Every department app is a Python app, so a host without a working Python
// cannot run this platform at all — and that has to be visible before a
// deploy, not discovered through one.
//
// Left to itself the failure surfaces as "필요한 패키지를 설치하지 못했습니다",
// which is not merely unhelpful but wrong: nothing is being installed, the
// interpreter is missing. A department reading that would edit
// requirements.txt over and over against a problem only an administrator can
// fix.

// pythonProbe resolves the interpreter once. The answer cannot change while
// this process runs, and the check spawns processes.
var pythonProbe = sync.OnceValues(func() (pythonInfo, error) {
	path, err := lookupTool("PYTHON_PATH", "python3")
	if err != nil {
		return pythonInfo{}, errors.New("python3 was not found — install it, or set [company] PYTHON_PATH to where it lives")
	}

	version, err := exec.Command(path, "-c", "import sys; print('.'.join(map(str, sys.version_info[:3])))").Output() //nolint:gosec // path comes from PATH or an admin-set config key
	if err != nil {
		return pythonInfo{}, fmt.Errorf("%s could not be run: %w", path, err)
	}

	// venv is a separate package on Debian and Ubuntu (python3-venv), so a
	// perfectly good python3 can still be unable to create an environment.
	// ensurepip goes with it and is what makes the new venv have a pip at
	// all. Checking here turns "the first deploy failed for no clear reason"
	// into a line in the startup log.
	if out, err := exec.Command(path, "-c", "import venv, ensurepip").CombinedOutput(); err != nil { //nolint:gosec // same
		return pythonInfo{}, fmt.Errorf("%s cannot create virtual environments — install python3-venv: %s",
			path, strings.TrimSpace(string(out)))
	}

	return pythonInfo{Path: path, Version: strings.TrimSpace(string(version))}, nil
})

type pythonInfo struct {
	Path    string
	Version string
}

// PythonStatus reports whether apps can be built on this host, for the admin
// page and the startup log.
func PythonStatus() (available bool, detail string) {
	info, err := pythonProbe()
	if err != nil {
		return false, err.Error()
	}
	return true, fmt.Sprintf("%s (%s)", info.Version, info.Path)
}

// errNoPython lets the deploy worker classify this as its own failure rather
// than as a dependency problem — a department told "필요한 패키지를 설치하지
// 못했습니다" would edit requirements.txt against a missing interpreter.
var errNoPython = errors.New("python is unavailable")

// pythonPath returns the interpreter to build with, or an error written for
// both audiences: a department cannot fix a missing interpreter, and saying
// so stops them trying.
func pythonPath() (string, error) {
	info, err := pythonProbe()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoPython, audienceError(
			"서버에 파이썬이 준비되어 있지 않아 앱을 빌드할 수 없습니다. "+
				"부서에서 고칠 수 있는 문제가 아니니 관리자에게 알려 주세요.",
			"파이썬을 사용할 수 없습니다: "+err.Error()))
	}
	return info.Path, nil
}
