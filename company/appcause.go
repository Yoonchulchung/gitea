// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

// A department can start and stop its own app, so it has to be able to find
// out why the app stopped — otherwise every failure becomes a question for
// an administrator and the boundary this platform draws stops meaning
// anything.
//
// But raw logs cannot be the answer: an app prints whatever it prints, which
// for a department app includes the data it handles, and everyone with read
// access to the repository would see it. So the department gets a
// *classified* cause in plain language, and the raw output stays on the
// admin pages.
//
// Each cause carries what to do next, because knowing the reason without
// knowing the remedy still ends in a support request. See
// docs/company/app-platform.md.

// AppCause is what a non-developer sees when their app is not working.
type AppCause struct {
	// Summary is written for someone who has never read a stack trace.
	Summary string
	// Detail is the app's own message where it is safe to show — a package
	// name, a limit. Never log output.
	Detail string
	// Action and ActionLabel point at the one thing that fixes it. Empty when
	// the fix is in the department's own code, where no button helps.
	Action      string
	ActionLabel string
}

// DepartmentCause explains an app's current problem, or returns nil when
// there isn't one. Nil is the normal case, and the screen shows nothing at
// all for it — a panel that is always saying something stops being read.
func DepartmentCause(st *AppState) *AppCause {
	if st.Actual != AppStateFailed && st.Actual != AppStateSuspended {
		return nil
	}
	switch st.Reason {
	case ReasonInstallFailed:
		return &AppCause{
			Summary: "필요한 패키지를 설치하지 못했습니다",
			Detail:  st.Message,
			// No button: a typo in requirements.txt is fixed in the file, and
			// offering an approval request here would send an admin a request
			// for a package that does not exist.
		}
	case ReasonPackageDenied:
		return &AppCause{
			Summary:     "아직 승인되지 않은 패키지가 있습니다",
			Detail:      st.Message,
			Action:      "deploy",
			ActionLabel: "패키지 승인 요청하기",
		}
	case ReasonOOM:
		return &AppCause{
			Summary:     "메모리를 한도보다 많이 사용해 중지되었습니다",
			Detail:      st.Message,
			Action:      "deploy",
			ActionLabel: "메모리 한도 상향 요청하기",
		}
	case ReasonHealthTimeout:
		return &AppCause{
			Summary: "앱이 시작된 뒤 응답하지 않았습니다",
			Detail:  "main.py 의 app 이 정상적으로 뜨는지, 시작하자마자 오류로 종료되지 않는지 확인해 주세요.",
		}
	case ReasonCrashLoop:
		return &AppCause{
			Summary: "앱이 반복해서 종료되어 자동 시작을 멈췄습니다",
			Detail:  "코드를 고친 뒤 다시 시작해 주세요. 계속 같은 문제가 나면 관리자에게 문의해 주세요.",
		}
	case ReasonSuspended:
		return &AppCause{
			Summary: "관리자가 이 앱을 정지시켰습니다",
			// The reason is the entire value here: "an administrator stopped
			// it" with no explanation leaves the department with nothing to
			// act on and no idea who to ask about what.
			Detail: st.Message,
		}
	case ReasonSandboxUnavailable:
		return &AppCause{
			Summary: "서버 설정 문제로 앱을 안전하게 실행할 수 없습니다",
			Detail:  "부서에서 고칠 수 있는 문제가 아닙니다. 관리자에게 문의해 주세요.",
		}
	case ReasonSecretError:
		return &AppCause{
			Summary:     "환경변수를 읽지 못했습니다",
			Detail:      "값을 다시 입력한 뒤 앱을 시작해 주세요.",
			Action:      "app",
			ActionLabel: "환경변수 다시 입력하기",
		}
	case ReasonDeployQueueFull:
		return &AppCause{
			Summary: "동시에 배포가 너무 많아 이번 배포가 처리되지 않았습니다",
			Detail:  "이전 버전은 그대로 동작하고 있습니다. 잠시 뒤 다시 배포해 주세요.",
		}
	case ReasonRolledBack:
		return &AppCause{
			Summary: "새 버전이 응답하지 않아 이전 버전으로 되돌렸습니다",
			Detail:  "지금 동작하는 것은 이전 버전입니다. 코드를 고쳐 다시 배포해 주세요.",
		}
	default:
		if st.Message == "" {
			return &AppCause{Summary: "앱이 실행되고 있지 않습니다"}
		}
		return &AppCause{Summary: "앱이 실행되고 있지 않습니다", Detail: st.Message}
	}
}
