// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

const (
	tplAdminApp     templates.TplName = "company/admin_app"
	tplAdminAppLogs templates.TplName = "company/admin_app_logs"
)

// windowFor turns the "?range=" picker into a start time. Bounded by the
// metrics retention period — offering a window with no data behind it just
// produces an empty chart nobody can explain.
func windowFor(value string) (time.Time, string) {
	switch value {
	case "1h":
		return time.Now().Add(-time.Hour), "1h"
	case "7d":
		return time.Now().AddDate(0, 0, -7), "7d"
	case "30d":
		return time.Now().AddDate(0, 0, -metricsRetentionDays), "30d"
	default:
		return time.Now().Add(-24 * time.Hour), "24h"
	}
}

// chartPoint is one plotted sample. Times are milliseconds because that is
// what chart.js's time scale expects.
type chartPoint struct {
	T        int64   `json:"t"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	P95      int     `json:"p95"`
	MemMB    float64 `json:"memMB"`
	CPU      float64 `json:"cpu"`
	Users    int     `json:"users"`
}

// chartAnnotation marks an event on the time axis — a deploy, a rollback, a
// forced stop.
//
// This is the single most useful thing on the page: without it an admin has
// to hold the deploy history in their head while reading the graph to answer
// "did this get worse after the last deploy?". With it, the answer is
// visible.
type chartAnnotation struct {
	T     int64  `json:"t"`
	Label string `json:"label"`
	Bad   bool   `json:"bad"`
}

// buildChartData renders the payload as JSON text for embedding in a page;
// buildChartPayload returns the same values for an endpoint that marshals
// them itself.
func buildChartData(buckets []MetricsBucket, st *AppState) (string, string) {
	points, annotations := buildChartPayload(buckets, st)
	pointsJSON, _ := json.Marshal(points)
	annotationsJSON, _ := json.Marshal(annotations)
	return string(pointsJSON), string(annotationsJSON)
}

func buildChartPayload(buckets []MetricsBucket, st *AppState) ([]chartPoint, []chartAnnotation) {
	points := make([]chartPoint, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, chartPoint{
			T:        b.T * 1000,
			Requests: b.Req.Total,
			Errors:   b.Req.S5xx,
			P95:      b.RT.P95,
			MemMB:    b.Mem.Max / (1 << 20),
			CPU:      b.CPU.Avg,
			Users:    b.Users,
		})
	}
	annotations := make([]chartAnnotation, 0, len(st.History))
	for _, h := range st.History {
		label, bad := historyLabel(h)
		if label == "" {
			continue
		}
		annotations = append(annotations, chartAnnotation{T: h.At * 1000, Label: label, Bad: bad})
	}
	return points, annotations
}

// historyLabel names an event for the chart. Returns "" for events not worth
// a line — a chart striped with markers is as unreadable as one with none.
func historyLabel(h AppHistoryEntry) (string, bool) {
	switch {
	case h.Reason == ReasonRolledBack:
		return "롤백", true
	case h.Reason == ReasonOOM:
		return "메모리 초과로 중지", true
	case h.Status == AppStateFailed:
		return "배포 실패", true
	case h.Status == AppStateSuspended:
		return "관리자 정지", true
	case h.Status == AppStateRunning && h.SHA != "":
		return "배포", false
	default:
		return "", false
	}
}

// adminAppContext resolves the app named in the URL, or writes a 404.
func adminAppContext(ctx *context.Context) (*AppState, *repo_model.Repository, bool) {
	owner := ctx.PathParam("owner")
	name := ctx.PathParam("repo")
	ref, known := LookupApp(owner, name)
	if !known {
		// Not every repo is an app, but an admin who followed a link here
		// expects a page, so fall back to the raw names rather than 404ing on
		// a repo that simply has never deployed.
		ref = AppRef{Owner: owner, Repo: name}
	}
	st := LoadAppState(ref.Owner, ref.Repo)
	// A missing repository is shown, not hidden: a renamed department repo
	// orphans its app, which keeps running at the old URL, and this page is
	// where that gets noticed.
	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ref.Owner, ref.Repo)
	if err != nil {
		repo = nil
	}
	return st, repo, true
}

// AdminApp is the per-app dashboard: state, controls, metrics, history.
func AdminApp(ctx *context.Context) {
	st, repo, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	since, window := windowFor(ctx.FormString("range"))
	buckets := LoadMetrics(st.Owner, st.Repo, since)
	summary := SummarizeMetrics(buckets)
	points, annotations := buildChartData(buckets, st)

	settings := SettingsFor(st.Owner, st.Repo)
	sandboxed, sandboxDetail := SandboxStatus()
	limitsOK, limitsDetail := LimitsStatus()

	envNames, envVersion, envErr := AppEnvNames(st.Owner, st.Repo)

	ctx.Data["Title"] = st.Owner + "/" + st.Repo
	ctx.Data["App"] = st
	// An admin needs three things about a failure, and the page showed none
	// of them: the classified cause, the raw detail behind it, and what the
	// department is being told — because "why is my app broken" arrives as a
	// question about that sentence, not about the log.
	ctx.Data["Cause"] = DepartmentCause(st)
	ctx.Data["AppRepo"] = repo
	ctx.Data["Settings"] = settings
	ctx.Data["Summary"] = summary
	ctx.Data["Window"] = window
	ctx.Data["ChartPoints"] = points
	ctx.Data["ChartAnnotations"] = annotations
	ctx.Data["MemoryLimitMB"] = settings.Limits.MemoryMB
	ctx.Data["Sandboxed"] = sandboxed
	ctx.Data["SandboxDetail"] = sandboxDetail
	ctx.Data["LimitsEnforced"] = limitsOK
	ctx.Data["LimitsDetail"] = limitsDetail
	// What this app may install, split by where the permission came from: the
	// platform's own stack is the same for everyone and is not this app's
	// decision, while allowExtra is what an admin approved for this app
	// specifically and is the list worth reviewing.
	ctx.Data["HistoryRows"] = describeHistory(st.History)
	ctx.Data["BasePackages"] = settings.BasePackages
	ctx.Data["SharedAllow"] = settings.Dependencies.Allow
	ctx.Data["ApprovedExtra"] = settings.Dependencies.AllowExtra
	// What the last build could not install. Offered for approval right here:
	// these are dependencies no department can name, so there is no form they
	// could raise to ask for them.
	ctx.Data["MissingPackages"] = st.MissingPackages
	ctx.Data["EnvNames"] = envNames
	ctx.Data["EnvVersion"] = envVersion
	ctx.Data["EnvError"] = envErr
	// A running process whose env predates the last save is using stale
	// values, and nothing about the app makes that visible from outside.
	ctx.Data["EnvRestartRequired"] = st.Actual == AppStateRunning && envVersion != st.EnvVersionRunning
	ctx.Data["AppURL"] = setting.AppSubURL + appProxyPrefix + "/" + st.Owner + "/" + st.Repo
	ctx.Data["AdminAppLink"] = setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo
	ctx.HTML(http.StatusOK, tplAdminApp)
}

// AdminAppLogs shows raw app output, with a search box.
//
// Admin-only, deliberately. An app prints whatever it prints, which for a
// department app can include the data it processes — so this is not
// something to hand to the whole department. They get a classified cause
// instead (docs/company/app-platform.md).
func AdminAppLogs(ctx *context.Context) {
	st, repo, ok := adminAppContext(ctx)
	if !ok {
		return
	}
	query := LogQuery{
		Text:   ctx.FormString("q"),
		Regexp: ctx.FormBool("regexp"),
		Limit:  ctx.FormInt("limit"),
	}
	lines, truncated, err := ReadAppLogs(st.Owner, st.Repo, query)

	ctx.Data["Title"] = st.Owner + "/" + st.Repo + " logs"
	ctx.Data["App"] = st
	ctx.Data["AppRepo"] = repo
	ctx.Data["Lines"] = lines
	ctx.Data["Truncated"] = truncated
	ctx.Data["Query"] = query.Text
	ctx.Data["UseRegexp"] = query.Regexp
	if err != nil {
		// A bad regexp is a user mistake, not a server error — say so and
		// keep the form on screen.
		ctx.Data["SearchError"] = err.Error()
	}
	ctx.Data["AdminAppLink"] = setting.AppSubURL + "/-/admin/company-deploys/" + st.Owner + "/" + st.Repo
	ctx.HTML(http.StatusOK, tplAdminAppLogs)
}

// AdminAppControl performs one lifecycle action as an administrator.
//
// Admins get the platform-protection verbs (suspend/resume/remove) on top of
// the ones a department has. The department's own start/stop lives on a
// different route with a different check — see company/app_control.go.
func AdminAppControl(ctx *context.Context) {
	owner := ctx.PathParam("owner")
	name := ctx.PathParam("repo")
	verb := ctx.PathParam("verb")
	actor := ctx.Doer.Name

	var err error
	switch verb {
	case "start":
		err = startAppAs(owner, name, actor, true)
	case "stop":
		err = StopApp(owner, name, actor, true)
	case "restart":
		err = RestartApp(owner, name, true)
	case "suspend":
		err = SuspendApp(owner, name, actor, ctx.FormString("reason"))
	case "resume":
		err = ResumeApp(owner, name, actor)
	case "rollback":
		err = RollbackApp(owner, name, actor)
	case "redeploy":
		err = RedeployApp(owner, name, actor, true)
	case "remove":
		err = RemoveApp(owner, name, actor)
	default:
		ctx.HTTPError(http.StatusBadRequest, "unknown action")
		return
	}

	if err != nil {
		// The administrator's own voice, and the raw error when nothing was
		// written for them. A department gets DepartmentSafeError instead —
		// safe to show is not the same as addressed to the reader
		// (company/usererror.go).
		ctx.Flash.Error(AdminError(err))
	} else {
		ctx.Flash.Success("완료되었습니다: " + verb)
	}
	if verb == "remove" {
		ctx.Redirect(setting.AppSubURL + "/-/admin/company-deploys")
		return
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + name)
}

// AdminAppMetrics serves the chart data on its own, so the range picker can
// swap it without reloading the page.
func AdminAppMetrics(ctx *context.Context) {
	owner := ctx.PathParam("owner")
	name := ctx.PathParam("repo")
	if _, known := LookupApp(owner, name); !known {
		ctx.JSON(http.StatusNotFound, map[string]string{"error": "unknown app"})
		return
	}
	since, _ := windowFor(ctx.FormString("range"))
	st := LoadAppState(owner, name)
	buckets := LoadMetrics(owner, name, since)
	points, annotations := buildChartPayload(buckets, st)
	summary := SummarizeMetrics(buckets)

	ctx.JSON(http.StatusOK, map[string]any{
		"points":      points,
		"annotations": annotations,
		"summary": map[string]any{
			"requests":  summary.Requests,
			"users":     summary.Users,
			"errorRate": strconv.FormatFloat(summary.ErrorRate, 'f', 2, 64),
			"p95":       summary.P95,
			"memMaxMB":  summary.MemMaxMB,
			"blocked":   summary.Blocked,
		},
	})
}

// AdminApprovePackages grants packages to one app straight from its page.
//
// Takes both the checked suggestions and a free-text field: the suggestions
// cover the case this exists for — a dependency the build discovered that
// nobody can request — and the field covers the admin who already knows what
// is needed and does not want to wait for a form to propose it.
func AdminApprovePackages(ctx *context.Context) {
	owner, repo := ctx.PathParam("owner"), ctx.PathParam("repo")

	// FormStrings, not ctx.Req.Form: the request body is not parsed until
	// something asks for a field, so reading the map directly returns nothing
	// and the approval silently does nothing at all.
	names := ctx.FormStrings("package")
	extra, problems := ParseBasePackages(ctx.FormString("extra"))
	if len(problems) > 0 {
		ctx.Flash.Error(strings.Join(problems, " / "))
		ctx.Redirect(setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + repo)
		return
	}
	names = append(names, extra...)

	if err := ApproveAppPackages(ctx, ctx.Doer, owner, repo, names); err != nil {
		ctx.Flash.Error(AdminError(err))
	} else {
		// Says what it did and what it did not: apps.yml is policy, and an
		// environment is built once, so nothing changes for the running app
		// until it is built again.
		ctx.Flash.Success("승인했습니다: " + strings.Join(names, ", ") + ". 적용하려면 [다시 배포]를 눌러 주세요.")
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/company-deploys/" + owner + "/" + repo)
}
