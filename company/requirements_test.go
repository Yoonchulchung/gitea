// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRequirementsAccepts(t *testing.T) {
	reqs, errs := ParseRequirements(strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"fastapi==0.115.0",
		"  uvicorn==0.30.6  ",
		"pandas==2.2.2  # inline comment",
		"Some_Pkg.Name==1.0",
	}, "\n"))

	require.Empty(t, errs)
	require.Len(t, reqs, 4)
	assert.Equal(t, "fastapi", reqs[0].Name)
	assert.Equal(t, "0.115.0", reqs[0].Version)
	assert.Equal(t, "uvicorn==0.30.6", reqs[1].Raw, "Raw keeps what pip is given, trimmed")
	// PEP 503: these are one package to pip, so they must be one string here
	// too — otherwise the allowlist is bypassable by spelling alone.
	assert.Equal(t, "some-pkg-name", reqs[3].Name)
}

// Each of these is a way to make pip fetch code from somewhere nobody
// approved — the "이상한 레포 가져오기" case this parser exists to stop.
func TestParseRequirementsRejectsRedirection(t *testing.T) {
	for _, line := range []string{
		"git+https://evil.example.com/backdoor.git",
		"https://evil.example.com/pkg.whl",
		"http://evil.example.com/pkg.whl",
		"file:///tmp/pkg",
		"-e ./local",
		"--editable ./local",
		"-r other-requirements.txt",
		"--requirement other.txt",
		"-i https://evil.example.com/simple",
		"--index-url https://evil.example.com/simple",
		"--extra-index-url https://evil.example.com/simple",
		"--find-links https://evil.example.com/",
		"-f https://evil.example.com/",
		"--trusted-host evil.example.com",
		"GIT+HTTPS://EVIL.EXAMPLE.COM/x.git", // case must not be an escape hatch
	} {
		t.Run(line, func(t *testing.T) {
			reqs, errs := ParseRequirements(line)
			assert.Empty(t, reqs, "nothing may be installed from this line")
			require.Len(t, errs, 1)
			assert.NotEmpty(t, errs[0].Reason)
		})
	}
}

func TestParseRequirementsRejectsUnpinned(t *testing.T) {
	// A range means the code that ships can change with nobody approving it
	// again, which would make approving a package meaningless.
	for _, line := range []string{
		"fastapi",
		"fastapi>=0.100",
		"fastapi~=0.115",
		"fastapi<1.0",
		"fastapi===0.1",
		"fastapi==",
		"==1.0",
		"fastapi==0.115.0; python_version < '3.12'", // environment markers
		"fastapi[all]==0.115.0",                     // extras
	} {
		t.Run(line, func(t *testing.T) {
			reqs, errs := ParseRequirements(line)
			assert.Empty(t, reqs)
			assert.Len(t, errs, 1)
		})
	}
}

func TestParseRequirementsReportsEveryBadLine(t *testing.T) {
	// All problems at once, so a department fixes them in one pass instead
	// of one failed deploy per typo.
	_, errs := ParseRequirements("fastapi\n-e .\nuvicorn==0.30.6\ngit+https://x/y.git")
	require.Len(t, errs, 3)
	assert.Equal(t, 1, errs[0].Line)
	assert.Equal(t, 2, errs[1].Line)
	assert.Equal(t, 4, errs[2].Line, "line numbers are 1-based and match the editor")
}

func TestDeniedPackages(t *testing.T) {
	reqs, errs := ParseRequirements("fastapi==0.115.0\nopenpyxl==3.1.5\nUvi_corn==0.30.6")
	require.Empty(t, errs)

	denied := DeniedPackages(reqs, []string{"fastapi", "uvi-corn"})
	require.Len(t, denied, 1)
	assert.Equal(t, "openpyxl", denied[0].Name)

	t.Run("allowlist matching is spelling-insensitive", func(t *testing.T) {
		// "Uvi_corn" in the file, "uvi.corn" on the list — same package.
		assert.Empty(t, DeniedPackages(reqs, []string{"fastapi", "openpyxl", "uvi.corn"}))
	})

	t.Run("an empty allowlist denies everything", func(t *testing.T) {
		// An app whose packages were never approved must not deploy just
		// because nobody has filled the list in yet.
		assert.Len(t, DeniedPackages(reqs, nil), 3)
	})
}
