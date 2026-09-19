// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/translation"
	gitea_context "gitea.dev/services/context"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// A department finds out that a file does not parse when the deploy fails,
// or worse, when a page comes up broken. The request form checks first:
// every Python file is parsed by the interpreter the build uses, every
// HTML file for tags that never close, every JavaScript file for syntax.
// The checks read; nothing is run.

const (
	checkMaxFiles     = 300
	checkMaxFileBytes = 1 << 20
	checkTimeout      = 20 * time.Second
)

// CheckFinding is one problem, at a line a person can go to.
type CheckFinding struct {
	Path    string
	Line    int
	Kind    string // "python", "html" or "js"
	Message string
}

// CheckReport is what the request form shows.
type CheckReport struct {
	Findings []CheckFinding
	Python   int // files checked, per kind
	HTML     int
	JS       int
	// PythonUnavailable is the platform having no interpreter: the Python
	// files were not checked, and the form says so rather than "no problems".
	PythonUnavailable bool
	// Truncated is more files than are looked at in one go.
	Truncated bool
}

// Problems reports whether there is anything to fix.
func (r CheckReport) Problems() bool { return len(r.Findings) > 0 }

// Checked is how many files were looked at; none means nothing to say.
func (r CheckReport) Checked() int { return r.Python + r.HTML + r.JS }

type checkFile struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// RunDeployChecks checks what the department's default branch holds now —
// the same files a deploy request would snapshot.
func RunDeployChecks(ctx *gitea_context.Context, repo *repo_model.Repository) CheckReport {
	var report CheckReport
	gitRepo, err := git.RepositoryFromRequestContextOrOpen(ctx, repo)
	if err != nil {
		return report
	}
	commit, err := gitRepo.GetBranchCommit(ctx, repo.DefaultBranch)
	if err != nil {
		return report // an empty repository: nothing to check yet
	}
	tree, err := commit.SubTree(ctx, gitRepo, "/")
	if err != nil {
		return report
	}
	entries, err := tree.ListEntriesRecursiveFast(ctx, gitRepo)
	if err != nil {
		return report
	}

	var pyFiles []checkFile
	seen := 0
	for _, e := range entries {
		if e.IsDir() || e.IsSubModule() || e.IsLink() {
			continue
		}
		kind := checkKind(e.Name())
		if kind == "" {
			continue
		}
		if seen == checkMaxFiles {
			report.Truncated = true
			break
		}
		seen++
		blob := e.Blob(gitRepo)
		if blob.Size(ctx) > checkMaxFileBytes {
			continue
		}
		content, err := blob.GetBlobBytes(ctx, blob.Size(ctx))
		if err != nil {
			continue
		}
		switch kind {
		case "python":
			pyFiles = append(pyFiles, checkFile{Path: e.Name(), Source: string(content)})
		case "html":
			report.HTML++
			report.Findings = append(report.Findings, checkHTML(e.Name(), content)...)
		case "js":
			report.JS++
			report.Findings = append(report.Findings, checkJS(e.Name(), content)...)
		}
	}
	if len(pyFiles) > 0 {
		findings, err := checkPython(ctx, ctx.Locale, pyFiles)
		if err != nil {
			report.PythonUnavailable = true
			log.Warn("company: deploy check: python files not checked: %v", err)
		} else {
			report.Python = len(pyFiles)
			report.Findings = append(report.Findings, findings...)
		}
	}
	sort.SliceStable(report.Findings, func(i, j int) bool {
		if report.Findings[i].Path != report.Findings[j].Path {
			return report.Findings[i].Path < report.Findings[j].Path
		}
		return report.Findings[i].Line < report.Findings[j].Line
	})
	return report
}

func checkKind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".py":
		return "python"
	case ".html", ".htm":
		return "html"
	case ".js", ".mjs":
		return "js"
	}
	return ""
}

// pythonCheckScript parses every file it is given and reports the first
// syntax error in each; a file that parses is then read for what would
// fail the moment it is imported — a name nothing defines, used by code
// that runs at import — and main.py for the app the platform starts.
// ast.parse compiles nothing and runs nothing. Names are gathered from the
// whole file, function bodies included, so a name bound anywhere counts as
// defined: that misses some failures and invents none.
const pythonCheckScript = `import ast, builtins, json, sys

KNOWN = set(dir(builtins)) | {"__name__", "__file__", "__doc__", "__builtins__", "__spec__", "__loader__", "__package__", "__annotations__", "__path__", "__cached__"}

def bound(tree):
    names, star = set(), False
    for node in ast.walk(tree):
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            for a in node.names:
                if a.name == "*": star = True
                else: names.add((a.asname or a.name).split(".")[0])
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)): names.add(node.name)
        elif isinstance(node, ast.Name) and not isinstance(node.ctx, ast.Load): names.add(node.id)
        elif isinstance(node, ast.arg): names.add(node.arg)
        elif isinstance(node, (ast.Global, ast.Nonlocal)): names.update(node.names)
        elif isinstance(node, ast.ExceptHandler) and node.name: names.add(node.name)
        elif isinstance(node, (ast.MatchAs, ast.MatchStar)) and node.name: names.add(node.name)
        elif isinstance(node, ast.MatchMapping) and node.rest: names.add(node.rest)
    return names, star

def undefined_at_import(tree, names):
    out, seen = [], set()
    def visit(node):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            for d in getattr(node, "decorator_list", []): visit(d)
            for d in node.args.defaults + [d for d in node.args.kw_defaults if d]: visit(d)
            return
        if isinstance(node, (ast.ListComp, ast.SetComp, ast.DictComp, ast.GeneratorExp)):
            visit(node.generators[0].iter)
            return
        if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Load) and node.id not in names and node.id not in KNOWN and node.id not in seen:
            seen.add(node.id)
            out.append((node.lineno, node.id))
        for child in ast.iter_child_nodes(node): visit(child)
    visit(tree)
    return out

out = []
for f in json.load(sys.stdin)["files"]:
    try:
        tree = ast.parse(f["source"], filename=f["path"])
    except SyntaxError as e:
        out.append({"path": f["path"], "line": e.lineno or 1, "message": e.msg}); continue
    except (ValueError, RecursionError) as e:
        out.append({"path": f["path"], "line": 1, "message": str(e)}); continue
    names, star = bound(tree)
    if not star:
        for line, name in undefined_at_import(tree, names):
            out.append({"path": f["path"], "line": line, "code": "undefined", "name": name})
    if f["path"] == "main.py" and "app" not in names:
        out.append({"path": f["path"], "line": 1, "code": "no_app"})
json.dump(out, sys.stdout)
`

func checkPython(ctx context.Context, locale translation.Locale, files []checkFile) ([]CheckFinding, error) {
	python, err := pythonPath()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python, "-I", "-c", pythonCheckScript) //nolint:gosec // the interpreter the build uses; the script is ours
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// A file crafted to keep the parser busy must not keep the form waiting.
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	timer := time.AfterFunc(checkTimeout, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var results []struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, err
	}
	findings := make([]CheckFinding, 0, len(results))
	for _, r := range results {
		message := r.Message
		switch r.Code {
		case "undefined":
			message = locale.TrString("company.check.undefined", r.Name)
		case "no_app":
			message = locale.TrString("company.check.no_app")
		}
		findings = append(findings, CheckFinding{Path: r.Path, Line: r.Line, Kind: "python", Message: message})
	}
	return findings, nil
}

// htmlOptionalEnd is what HTML lets a page leave open: a <p> before the
// next block, a <li> before the next item. Reporting those would flag pages
// that display perfectly well.
var htmlOptionalEnd = map[atom.Atom]bool{
	atom.Html: true, atom.Head: true, atom.Body: true, atom.P: true, atom.Li: true,
	atom.Dt: true, atom.Dd: true, atom.Tr: true, atom.Td: true, atom.Th: true,
	atom.Thead: true, atom.Tbody: true, atom.Tfoot: true, atom.Colgroup: true,
	atom.Option: true, atom.Optgroup: true, atom.Rt: true, atom.Rp: true,
}

// htmlVoid never has an end tag.
var htmlVoid = map[atom.Atom]bool{
	atom.Area: true, atom.Base: true, atom.Br: true, atom.Col: true, atom.Embed: true,
	atom.Hr: true, atom.Img: true, atom.Input: true, atom.Link: true, atom.Meta: true,
	atom.Source: true, atom.Track: true, atom.Wbr: true, atom.Param: true,
}

type openTag struct {
	name string
	atom atom.Atom
	line int
}

// checkHTML finds the two mistakes a browser hides by guessing: an element
// opened and never closed, and an end tag that closes nothing. The parser
// browsers use would build a tree from either, so the tokenizer is walked
// instead and the tags matched by hand.
func checkHTML(name string, content []byte) []CheckFinding {
	var findings []CheckFinding
	var stack []openTag
	z := html.NewTokenizer(bytes.NewReader(content))
	line := 1
	for {
		tt := z.Next()
		at := line
		line += bytes.Count(z.Raw(), []byte{'\n'})
		switch tt {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) {
				findings = append(findings, CheckFinding{Path: name, Line: at, Kind: "html", Message: z.Err().Error()})
			}
			for _, open := range slices.Backward(stack) {
				if !htmlOptionalEnd[open.atom] {
					findings = append(findings, CheckFinding{Path: name, Line: open.line, Kind: "html", Message: fmt.Sprintf("<%s> is never closed", open.name)})
				}
			}
			return findings
		case html.StartTagToken:
			tn, _ := z.TagName()
			a := atom.Lookup(tn)
			if htmlVoid[a] {
				continue
			}
			stack = append(stack, openTag{name: string(tn), atom: a, line: at})
		case html.EndTagToken:
			tn, _ := z.TagName()
			a := atom.Lookup(tn)
			if htmlVoid[a] {
				continue // </br> and friends: browsers ignore them
			}
			idx := -1
			for i, open := range slices.Backward(stack) {
				if open.atom == a && (a != 0 || open.name == string(tn)) {
					idx = i
					break
				}
			}
			if idx == -1 {
				findings = append(findings, CheckFinding{Path: name, Line: at, Kind: "html", Message: fmt.Sprintf("</%s> closes nothing — there is no open <%s>", tn, tn)})
				continue
			}
			for _, open := range slices.Backward(stack[idx+1:]) {
				if !htmlOptionalEnd[open.atom] {
					findings = append(findings, CheckFinding{Path: name, Line: open.line, Kind: "html", Message: fmt.Sprintf("<%s> is not closed before </%s> on line %d", open.name, tn, at)})
				}
			}
			stack = stack[:idx]
		}
	}
}

// checkJS parses the file as a script or module and reports where that
// stops. The parser follows the current language, so modern syntax passes.
func checkJS(name string, content []byte) []CheckFinding {
	_, err := js.Parse(parse.NewInput(bytes.NewReader(content)), js.Options{})
	if err == nil {
		return nil
	}
	line := 1
	message := err.Error()
	if perr, ok := errors.AsType[*parse.Error](err); ok {
		line, _, _ = perr.Position()
		message = perr.Message
	}
	return []CheckFinding{{Path: name, Line: line, Kind: "js", Message: message}}
}
