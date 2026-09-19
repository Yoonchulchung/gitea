// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"fmt"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/json"
	gitea_context "gitea.dev/services/context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What the workspace assistant can see of the app itself. "It does not
// start" used to be answered by asking the employee to copy a log line into
// the chat; these tools let the assistant read what the app page and the
// request form show — the state and why the platform stopped it, the log,
// the checks — and, under a policy, have the startup check run.
//
// Running is the one tool that executes code, and the code it executes is
// the committed branch, never a proposal the assistant has staged: those
// exist only until the employee saves. So the assistant cannot write
// something and run it in one move; a person is between the two, as with
// every other change. And it runs only inside an enforced sandbox: where a
// host lets people run apps without one (a developer's machine, an operator's
// opt-in), the assistant still may not — the sandbox is the only reason the
// run is safe to offer to something that takes instructions from file
// contents.

const (
	aiLogLinesDefault = 80
	aiLogLinesMax     = 300
)

// aiMayRunStartupCheck is the policy: an enforced sandbox, and the setting
// not switched off.
func aiMayRunStartupCheck() (bool, string) {
	if mode, detail := sandboxMode(); mode == SandboxNone {
		return false, "this server does not isolate apps (" + detail + "), and the assistant runs code only inside a sandbox"
	}
	if strings.EqualFold(strings.TrimSpace(companySetting("AI_MAY_RUN_STARTUP_CHECK")), "false") {
		return false, "an administrator has switched this off ([company] AI_MAY_RUN_STARTUP_CHECK)"
	}
	return true, ""
}

func clampLogLines(n int) int {
	if n <= 0 {
		return aiLogLinesDefault
	}
	return min(n, aiLogLinesMax)
}

// addAppInsightTools gives the workspace server the app's own facts.
func addAppInsightTools(server *mcp.Server, ctx *gitea_context.Context, repo *repo_model.Repository) {
	owner, name := repo.OwnerName, repo.Name
	logAccess := AILogAccess()

	server.AddTool(&mcp.Tool{
		Name:        "app_status",
		Description: "The deployed app's state: running or stopped, why the platform stopped it, the last failure and its traceback line, health, memory and data against their limits, and the policy it runs under (network, downloads, access). Call this first when the employee says the app is down or broken.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult(appStatusForAI(ctx, repo)), nil
	})

	logsDescription := "The app's own output, newest last: print() lines, uvicorn's log, tracebacks. `lines` is how many (default 80, at most 300); `search` keeps only lines containing that text."
	if logAccess == AILogAccessErrors {
		logsDescription = "The failures in the app's own output, newest last: tracebacks and error lines only (policy: ordinary output stays on the server). `lines` is how many lines to scan (default 80, at most 300); `search` keeps only lines containing that text."
	}
	if logAccess != AILogAccessOff {
		server.AddTool(&mcp.Tool{
			Name:        "app_logs",
			Description: logsDescription,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"lines":  map[string]any{"type": "integer"},
					"search": map[string]any{"type": "string"},
				},
			},
		}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Lines  int    `json:"lines"`
				Search string `json:"search"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult(err), nil
			}
			lines, truncated, err := ReadAppLogs(owner, name, LogQuery{Text: args.Search, Limit: clampLogLines(args.Lines)})
			if err != nil {
				return errorResult(err), nil
			}
			if len(lines) == 0 {
				return textResult("The app has not written anything yet (or has never been started)."), nil
			}
			// The policy decides which lines leave, and masks what does
			// (company/ailogpolicy.go).
			kept := aiLogLines(owner, name, lines)
			if len(kept) == 0 {
				return textResult("No failures in the lines scanned. (Ordinary output is not shared with the assistant on this server.)"), nil
			}
			var b strings.Builder
			for _, l := range kept {
				if l.At > 0 {
					b.WriteString(time.Unix(l.At, 0).Format("01-02 15:04:05 "))
				}
				if l.Build {
					b.WriteString("[build] ")
				}
				b.WriteString(l.Text)
				b.WriteString("\n")
			}
			if truncated {
				b.WriteString("(older lines not shown)\n")
			}
			return textResult(b.String()), nil
		})
	}

	server.AddTool(&mcp.Tool{
		Name:        "deploy_checks",
		Description: "What the deploy request form shows for the COMMITTED branch: the pre-deploy check (syntax, names that fail on import, main.py's app, HTML/JS), the preflight (Python, requirements, package approval) and the startup check's latest result. Unsaved proposals are not included.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult(deployChecksForAI(ctx, repo)), nil
	})

	server.AddTool(&mcp.Tool{
		Name:        "run_startup_check",
		Description: "Build and start the COMMITTED branch once, in the platform's sandbox, the way a deploy would — import main, find app, finish startup, answer the health path — and report. Takes seconds to minutes; if the result says it is still running, wait and call deploy_checks. It never runs proposals you staged with write_file: the employee must save first. Refused on a server without an enforced sandbox.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if ok, why := aiMayRunStartupCheck(); !ok {
			return textResult("Not run: " + why + ". Ask the employee to save and open the deploy request page — the platform runs the same check there, and deploy_checks shows its result."), nil
		}
		check, ok := EnsureStartupCheck(ctx, repo, true)
		if !ok {
			return textResult("Nothing to run: the repository has no commits yet."), nil
		}
		return textResult("Started on commit " + shortSHA(check.SHA) + ". " + startupCheckForAI(forAIPolicy(check))), nil
	})
}

// forAIPolicy is a check's record with its app output under the policy:
// the traceback is an error and may go where errors go, the output only
// where everything may.
func forAIPolicy(c StartupCheck) StartupCheck {
	c.Error = aiErrorText(c.Owner, c.Repo, c.Error)
	c.Output = aiOutputText(c.Owner, c.Repo, c.Output)
	if AILogAccess() == AILogAccessOff && c.State == "failed" {
		c.Error = "(the app's output is withheld by policy; the stage says where it stopped)"
	}
	return c
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// appStatusForAI is the app page, as text.
func appStatusForAI(ctx *gitea_context.Context, repo *repo_model.Repository) string {
	owner, name := repo.OwnerName, repo.Name
	tr := ctx.Locale.TrString
	settings := SettingsFor(owner, name)
	var b strings.Builder
	if _, deployed := LookupApp(owner, name); !deployed {
		b.WriteString("Not deployed yet: nothing runs until a deploy request is approved.\n")
	} else {
		st := LoadAppState(owner, name)
		fmt.Fprintf(&b, "State: %s (actual %q, wanted %q), sandboxed: %t.\n", tr(departmentStatusLabel(st)), st.Actual, st.Desired, st.Sandboxed)
		if st.Reason != "" {
			reason := st.Reason
			if key := historyReasonLabel(st.Reason); key != "" {
				reason = tr(key)
			}
			fmt.Fprintf(&b, "Why: %s.\n", reason)
		}
		switch {
		case st.UserMessageKey != "":
			fmt.Fprintf(&b, "Message shown to the department: %s\n", tr(st.UserMessageKey))
		case st.UserMessage != "":
			fmt.Fprintf(&b, "Message shown to the department: %s\n", aiErrorText(owner, name, st.UserMessage))
		}
		if st.FailedAt > 0 {
			fmt.Fprintf(&b, "Failed at: %s.\n", time.Unix(st.FailedAt, 0).Format(time.RFC3339))
		}
		if cause := DepartmentCause(st); cause != nil {
			fmt.Fprintf(&b, "Cause: %s", tr(cause.Summary))
			switch {
			case cause.Detail != "" && cause.DetailArg != nil:
				fmt.Fprintf(&b, " — %s", tr(cause.Detail, cause.DetailArg))
			case cause.Detail != "":
				fmt.Fprintf(&b, " — %s", tr(cause.Detail))
			case cause.DetailText != "":
				fmt.Fprintf(&b, " — %s", aiErrorText(owner, name, cause.DetailText))
			}
			b.WriteString("\n")
			if finding := LogFindingFor(st, cause); finding != nil && finding.Exception != "" {
				// Under "off" the exception's words stay on the server; the
				// place in the code still helps.
				if text := aiErrorText(owner, name, finding.Exception); text != "" {
					fmt.Fprintf(&b, "Last exception: %s", text)
				} else {
					b.WriteString("Last exception: (withheld by policy)")
				}
				if finding.File != "" {
					fmt.Fprintf(&b, " (in %s line %d, %s)", finding.File, finding.Line, finding.Func)
				}
				b.WriteString("\n")
			}
		}
		fmt.Fprintf(&b, "Health: %s.\n", st.Health.State)
		if st.Actual == AppStateRunning {
			fmt.Fprintf(&b, "Memory: %d MB now, limit %d MB.\n", CurrentMemoryMB(owner, name), settings.Limits.MemoryMB)
		}
		if usage, ok := AppDataUsageFor(ctx, owner, name); ok && usage.QuotaBytes > 0 {
			fmt.Fprintf(&b, "Data: %d MB of %d MB (%d%%).\n", usage.Bytes>>20, usage.QuotaBytes>>20, usage.Percent)
		}
	}
	fmt.Fprintf(&b, "Policy: network %q, downloads %q, access %q, health path %s, CPU limit %d%%, data limit %d MB.\n",
		settings.Network.Mode, settings.Download.Policy, settings.Access, settings.HealthPath, settings.Limits.CPUPercent, int(appDataQuotaBytes(settings)>>20))
	if len(settings.Dependencies.AllowExtra) > 0 {
		fmt.Fprintf(&b, "Packages approved for this app beyond the base stack: %s.\n", strings.Join(settings.Dependencies.AllowExtra, ", "))
	}
	return b.String()
}

// deployChecksForAI is the request form's three checks, as text.
func deployChecksForAI(ctx *gitea_context.Context, repo *repo_model.Repository) string {
	var b strings.Builder
	report := RunDeployChecks(ctx, repo)
	switch {
	case report.Problems():
		fmt.Fprintf(&b, "Pre-deploy check: %d problems.\n", len(report.Findings))
		for _, f := range report.Findings {
			fmt.Fprintf(&b, "- %s:%d — %s\n", f.Path, f.Line, f.Message)
		}
	case report.Checked() > 0:
		fmt.Fprintf(&b, "Pre-deploy check: no problems in %d Python, %d HTML, %d JavaScript files.\n", report.Python, report.HTML, report.JS)
	default:
		b.WriteString("Pre-deploy check: nothing to check yet.\n")
	}
	if report.PythonUnavailable {
		b.WriteString("(Python files were not checked: the server has no interpreter.)\n")
	}

	pre := runPreflight(ctx, repo)
	fmt.Fprintf(&b, "\nPreflight: %s\n", pre.Summary)
	for _, c := range pre.Checks {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", c.Status, c.Label, aiErrorText(repo.OwnerName, repo.Name, c.Detail))
	}
	if len(pre.NeedsApproval) > 0 {
		fmt.Fprintf(&b, "Packages waiting for an administrator's approval: %s\n", strings.Join(pre.NeedsApproval, ", "))
	}

	b.WriteString("\nStartup check: ")
	if check, ok := EnsureStartupCheck(ctx, repo, false); ok {
		b.WriteString(startupCheckForAI(forAIPolicy(check)))
	} else {
		b.WriteString("nothing to run yet.")
	}
	return b.String()
}

func startupCheckForAI(c StartupCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", c.State)
	if c.Stage != "" {
		fmt.Fprintf(&b, " at stage %q", c.Stage)
	}
	fmt.Fprintf(&b, " (commit %s", shortSHA(c.SHA))
	if !c.FinishedAt.IsZero() {
		fmt.Fprintf(&b, ", finished %s, took %ds", c.FinishedAt.Format(time.RFC3339), c.Seconds())
	}
	b.WriteString(").\n")
	if c.Error != "" {
		b.WriteString(c.Error)
		b.WriteString("\n")
	}
	if c.Output != "" {
		b.WriteString("App output:\n")
		b.WriteString(c.Output)
		b.WriteString("\n")
	}
	return b.String()
}
