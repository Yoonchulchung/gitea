// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"
	"strings"

	"gitea.dev/models/unit"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/services/context"
)

// The department's own view of its app, at /{owner}/{repo}/_app.
//
// Named _app rather than _deploy because /{owner}/{repo}/deploy already
// exists (the Deploy Request form), and two routes one underscore apart
// would confuse the non-developers this platform is for as much as the
// people maintaining it. The underscore prefix follows this fork's existing
// convention for company screens (_edits, _edits_ai, _edits_tmp).
//
// Everything here is phrased for someone who is not a developer: what the
// app is doing, why it stopped, and what they can do about it. Raw logs are
// deliberately absent — those are on the admin pages, because an app's
// output can contain the data it handles. See docs/company/app-platform.md.

const (
	tplApp     templates.TplName = "company/app"
	tplAppLogs templates.TplName = "company/app_logs"
)

// AppPage renders the department's app screen.
func AppPage(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	st := LoadAppState(owner, name)
	settings := SettingsFor(owner, name)
	envNames, envVersion, envErr := AppEnvNames(owner, name)

	canWrite := ctx.Repo.Permission.CanWrite(unit.TypeCode)

	ctx.Data["Title"] = "앱 관리"
	ctx.Data["App"] = st
	ctx.Data["Settings"] = settings
	ctx.Data["StatusLabel"] = departmentStatusLabel(st)
	ctx.Data["CauseSummary"] = DepartmentCause(st)
	ctx.Data["EnvNames"] = envNames
	ctx.Data["EnvVersion"] = envVersion
	// A read failure here is a filesystem error carrying a path; the
	// department gets the fact, an admin gets the detail in the log.
	if envErr != nil {
		ctx.Data["EnvError"] = DepartmentSafeError("reading env for "+owner+"/"+name, envErr)
	}
	ctx.Data["EnvRestartRequired"] = st.Actual == AppStateRunning && envVersion != st.EnvVersionRunning
	ctx.Data["EnvHistory"] = AppEnvHistory(owner, name)
	ctx.Data["CanControl"] = canWrite
	ctx.Data["AppURL"] = setting.AppSubURL + appProxyPrefix + "/" + owner + "/" + name
	ctx.Data["AppLink"] = ctx.Repo.RepoLink + "/_app"
	ctx.Data["DeployLink"] = ctx.Repo.RepoLink + "/deploy"
	// The department may narrow access on their own but not widen it —
	// widening is a Deploy Request, because it exposes their data further.
	ctx.Data["AccessOptions"] = accessOptionsFor(settings.Access, configuredAccess(owner, name))
	ctx.Data["BlankEnvRows"] = blankEnvRowIndexes
	// "이전 버전으로" is irreversible in the sense that matters — it takes the
	// app off what is working now — so both ends of that swap are named here
	// rather than left for the person to remember.
	ctx.Data["Running"] = CurrentRelease(owner, name)
	ctx.Data["Previous"] = PreviousRelease(owner, name)
	ctx.Data["HistoryRows"] = describeHistory(st.History)
	// Surfaced on its own rather than through DepartmentCause, which keys on
	// st.Reason — and that is cleared as soon as another deploy is queued,
	// taking the only explanation of why nothing installs with it.
	ctx.Data["MissingPackages"] = st.MissingPackages
	ctx.HTML(http.StatusOK, tplApp)
}

// AppAccessSave applies a department's access choice.
func AppAccessSave(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name
	if err := SetDepartmentAccess(owner, name, ctx.Doer.Name, ctx.FormString("access")); err != nil {
		ctx.Flash.Error(DepartmentSafeError("setting access for "+owner+"/"+name, err))
	} else {
		ctx.Flash.Success("접근 범위를 변경했습니다. 바로 적용됩니다.")
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/_app")
}

// AppControl handles a department's start/stop/restart/rollback.
func AppControl(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name
	actor := ctx.Doer.Name
	isAdmin := ctx.Doer.IsAdmin

	var err error
	switch ctx.PathParam("verb") {
	case "start":
		err = startAppAs(owner, name, actor, isAdmin)
	case "stop":
		err = StopApp(owner, name, actor, isAdmin)
	case "restart":
		err = RestartAppAs(owner, name, actor, isAdmin)
	case "rollback":
		err = RollbackApp(owner, name, actor)
	case "redeploy":
		err = RedeployApp(owner, name, actor, isAdmin)
	default:
		ctx.HTTPError(http.StatusBadRequest, "unknown action")
		return
	}
	if err != nil {
		// Never err.Error(): an os error carries the absolute path it failed
		// on, which is inside Gitea's data directory (company/usererror.go).
		ctx.Flash.Error(DepartmentSafeError(ctx.PathParam("verb")+" "+owner+"/"+name, err))
	}
	// Back where the button was pressed. These controls sit on the repository
	// header as well as this screen, and bouncing someone to a different page
	// for clicking "stop" reads as though something went wrong.
	// RedirectToCurrentSite refuses an off-site referer, so an attacker cannot
	// turn this into an open redirect.
	ctx.RedirectToCurrentSite(ctx.Req.Referer(), ctx.Repo.RepoLink+"/_app")
}

// AppEnvSave stores the app's environment variables.
//
// A blank value means "leave this one alone", matching the (unchanged)
// convention the AI settings screen already uses, so a department never has
// to retype a secret they cannot see. Removal is explicit, via the delete
// checkbox, so a blank field can never silently drop a credential.
func AppEnvSave(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	// Parsed before the map is read: ctx.Req.Form is nil until a field is
	// asked for, so ranging over it first found nothing and every save was a
	// no-op — the form came back empty and no variable was ever stored.
	if err := ctx.Req.ParseForm(); err != nil {
		ctx.Flash.Error("입력을 읽지 못했습니다. 다시 시도해 주세요.")
		ctx.Redirect(ctx.Repo.RepoLink + "/_app")
		return
	}

	set := map[string]string{}
	var unset []string
	form := ctx.Req.Form
	for key, values := range form {
		envName, ok := strings.CutPrefix(key, "env_name_")
		if !ok {
			continue
		}
		varName := strings.TrimSpace(values[0])
		if varName == "" {
			continue
		}
		if form.Get("env_delete_"+envName) != "" {
			unset = append(unset, varName)
			continue
		}
		if value := form.Get("env_value_" + envName); value != "" {
			set[varName] = value
		}
	}

	if len(set) == 0 && len(unset) == 0 {
		ctx.Flash.Info("변경된 항목이 없습니다.")
		ctx.Redirect(ctx.Repo.RepoLink + "/_app")
		return
	}
	if err := SaveAppEnv(owner, name, ctx.Doer.Name, set, unset); err != nil {
		ctx.Flash.Error(DepartmentSafeError("saving env for "+owner+"/"+name, err))
	} else {
		// The trap this warning exists for: a process's environment cannot be
		// changed while it runs, so without saying so the department changes a
		// value, sees no effect, and spends an afternoon on it.
		ctx.Flash.Success("저장했습니다. 변경사항을 적용하려면 앱을 재시작하세요.")
	}
	ctx.Redirect(ctx.Repo.RepoLink + "/_app")
}

// accessOption is one choice on the access-mode picker, described by what it
// does rather than by its name — a non-developer should be choosing an
// outcome, not a keyword.
type accessOption struct {
	Value    string
	Label    string
	Explain  string
	Selected bool
	// Widening exposes the app to more people, so it needs an admin's
	// approval and is offered as a request rather than a switch.
	NeedsRequest bool
}

// accessRank orders the modes from most restrictive to least. Narrowing is
// immediate; widening goes through a Deploy Request.
var accessRank = map[string]int{AccessOrg: 0, AccessLogin: 1, AccessPublic: 2}

// accessOptionsFor describes the picker. `current` is what is in force;
// `ceiling` is what policy permits, which is what decides whether an option is
// a switch the department can flip or a request they have to raise.
func accessOptionsFor(current, ceiling string) []accessOption {
	if current == "" {
		current = AccessPublic
	}
	options := []accessOption{
		{Value: AccessOrg, Label: "우리 부서만", Explain: "이 저장소를 소유한 조직의 구성원만 볼 수 있습니다"},
		{Value: AccessLogin, Label: "로그인한 사람만", Explain: "Gitea 계정이 있는 사내 구성원이면 볼 수 있습니다"},
		{Value: AccessPublic, Label: "사내 누구나", Explain: "주소를 아는 사람은 로그인 없이 볼 수 있습니다"},
	}
	for i := range options {
		options[i].Selected = options[i].Value == current
		options[i].NeedsRequest = accessRank[options[i].Value] > accessRank[ceiling]
	}
	return options
}

// blankEnvRowIndexes are the empty rows offered for new variables. An app
// normally needs several at once — a database URL and its credentials arrive
// together — and a form with room for one turns that into one save, one
// restart prompt, and one reload per value.
var blankEnvRowIndexes = []string{"new0", "new1", "new2", "new3", "new4"}

// historyRow is one line of "최근 이력", written out rather than left as the
// state machine's own vocabulary. `activating`, `rolled_back` and
// `contract_violation` are precise and mean nothing to the person reading
// them; the columns exist so it is clear which part is when, who and what.
type historyRow struct {
	At     int64
	Actor  string
	What   string
	Detail string
	SHA    string
	// Status and Reason are the untranslated codes. Shown only on admin
	// screens, where they are what matches a line against the logs and the
	// source; a department reading them learns nothing.
	Status string
	Reason string
}

func describeHistory(entries []AppHistoryEntry) []historyRow {
	rows := make([]historyRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, historyRow{
			At: e.At, Actor: e.Actor, SHA: e.SHA,
			What:   historyStatusLabel(e.Status),
			Detail: historyReasonLabel(e.Reason),
			Status: e.Status,
			Reason: e.Reason,
		})
	}
	return rows
}

func historyStatusLabel(status string) string {
	switch status {
	case AppStateRunning:
		return "실행"
	case AppStateStopped:
		return "중지"
	case AppStateSuspended:
		return "관리자 정지"
	case AppStateFailed:
		return "실패"
	case AppStateQueued, AppStateBuilding, AppStateActivating:
		return "배포"
	default:
		return status
	}
}

// historyReasonLabel keeps unmapped reasons visible rather than blanking them:
// an unfamiliar code is still a clue, while an empty cell is not.
func historyReasonLabel(reason string) string {
	if mode, ok := strings.CutPrefix(reason, ReasonAccessChanged+":"); ok {
		switch mode {
		case AccessOrg:
			return "접근 범위를 '우리 부서만'으로 변경"
		case AccessLogin:
			return "접근 범위를 '로그인한 사람만'으로 변경"
		default:
			return "접근 범위를 '사내 누구나'로 변경"
		}
	}
	switch reason {
	case "":
		return ""
	case ReasonRolledBack:
		return "문제가 있어 이전 버전으로 되돌림"
	case ReasonInstallFailed:
		return "필요한 패키지를 설치하지 못함"
	case ReasonPackageDenied:
		return "승인되지 않은 패키지"
	case ReasonOOM:
		return "메모리 한도 초과"
	case ReasonHealthTimeout:
		return "시작 후 응답이 없음"
	case ReasonCrashLoop:
		return "반복 종료되어 자동 시작 중단"
	case ReasonSuspended:
		return "관리자가 정지시킴"
	case ReasonNoRelease:
		return "실행할 수 있는 버전이 없음"
	case ReasonNoPython:
		return "서버에 파이썬이 준비되지 않음"
	case ReasonContractViolation:
		return "main.py 를 찾을 수 없음"
	case ReasonDeployQueueFull:
		return "배포 대기열이 가득 참"
	case ReasonVersionPinned:
		return "특정 버전을 지정해 배포"
	case "redeploy":
		return "다시 배포 요청"
	case "restarted":
		return "재시작"
	case "resumed":
		return "관리자가 정지를 해제"
	case "deployed while stopped":
		return "중지 상태에서 새 버전만 준비됨"
	default:
		return reason
	}
}

// departmentStatusLabel is the one word a non-developer sees. rolled_back
// never reaches them: to the department the app either works or it doesn't.
func departmentStatusLabel(st *AppState) string {
	switch st.Actual {
	case AppStateRunning:
		return "실행 중"
	case AppStateQueued, AppStateBuilding, AppStateActivating:
		return "배포 중"
	case AppStateSuspended:
		return "관리자가 정지시킴"
	case AppStateFailed:
		return "문제 발생"
	default:
		return "중지됨"
	}
}

// AppLogs shows a department its own app's output.
//
// This was admin-only at first, on the grounds that an app prints whatever it
// prints and that can include the data it handles. That reasoning does not
// survive the permission it is gated on: everyone who reaches this page can
// already push code to the app, and someone who can change what it prints
// does not need a log viewer to read its data. Withholding the logs from
// them protected nothing and sent every failure to an administrator.
//
// Read-only members of the repository are a different matter and still see
// none of this — the route requires write access, not read.
func AppLogs(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	query := LogQuery{
		Text:   ctx.FormString("q"),
		Regexp: ctx.FormBool("regexp"),
		Limit:  ctx.FormInt("limit"),
	}
	lines, truncated, err := ReadAppLogs(owner, name, query)

	// The app's own output, so it names the server's paths — a Python
	// traceback prints the absolute location of the venv and of the release
	// tree, both of which sit inside the directory the sandbox exists to
	// hide. Redacted for a department; an admin's log view leaves them
	// alone, because there the paths are the point.
	//
	// After the search, not before: someone looking for a package name still
	// matches against what the app actually wrote.
	for i := range lines {
		lines[i].Text = RedactServerPaths(lines[i].Text)
	}

	ctx.Data["Title"] = "앱 로그"
	ctx.Data["App"] = LoadAppState(owner, name)
	ctx.Data["Lines"] = lines
	ctx.Data["Truncated"] = truncated
	ctx.Data["Query"] = query.Text
	ctx.Data["UseRegexp"] = query.Regexp
	if err != nil {
		// A bad regexp is the reader's own typo, not a server error.
		ctx.Data["SearchError"] = err.Error()
	}
	ctx.Data["AppLink"] = ctx.Repo.RepoLink + "/_app"
	ctx.HTML(http.StatusOK, tplAppLogs)
}

const tplAppHistory templates.TplName = "company/app_history"

// AppHistory shows a department every deploy their app has had.
//
// The state file keeps only the last ten events, because it doubles as the
// rollback index. That is the right size for "what is happening now" and the
// wrong one for "when did this last work" — which is the question someone
// asks after a deploy fails, and the one they currently take to an
// administrator.
//
// Same source as the admin view, the build log's own headers, so the two
// screens cannot disagree about what happened. The one difference is
// redaction: a failure summary is pip's output and names the server's paths,
// which are inside the directory the sandbox exists to hide.
func AppHistory(ctx *context.Context) {
	owner := ctx.Repo.Owner.Name
	name := ctx.Repo.Repository.Name

	attempts := readDeployAttempts(appPathsFor(owner, name))
	for i := range attempts {
		attempts[i].Summary = RedactServerPaths(attempts[i].Summary)
	}
	running := CurrentRelease(owner, name)
	markCurrent(attempts, running.SHA)

	page := max(ctx.FormInt("page"), 1)
	const perPage = 20
	start := min((page-1)*perPage, len(attempts))
	end := min(start+perPage, len(attempts))

	st := LoadAppState(owner, name)
	ctx.Data["Title"] = "배포 이력"
	ctx.Data["App"] = st
	ctx.Data["Attempts"] = attempts[start:end]
	ctx.Data["TotalAttempts"] = len(attempts)
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(int64(len(attempts))).PerPageLimit(perPage).CurPage(page).Build()
	ctx.Data["HistoryRows"] = describeHistory(st.History)
	ctx.Data["Running"] = running
	ctx.Data["AppLink"] = ctx.Repo.RepoLink + "/_app"
	ctx.HTML(http.StatusOK, tplAppHistory)
}
