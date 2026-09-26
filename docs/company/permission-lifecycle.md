# 권한의 수명주기 — 요청, 부분 승인, 반납

승인된 권한은 `.company/apps.yml`에 커밋되고, 그 커밋이 감사 기록이다
([permissions_commit.go](../../company/permissions_commit.go) 첫 주석). 이
문서는 그 파일에 무엇이 들어갈지를 **누가, 어느 시점에, 어떤 화면에서**
정하는지 — 지금 어떻게 되어 있고, 무엇이 빠져 있고, 어떻게 채울지 — 를
적는다. 구현하면서 아래 체크박스를 갱신한다.

배경은 [app-platform.md](app-platform.md)의 "넓히는 변경은 승인, 좁히는
변경은 즉시" 원칙이고, 이 문서는 그 원칙을 끝까지 적용하는 작업이다. 지금은
넓히는 쪽만 구현되어 있다.

## 지금 실제로 일어나는 일 (코드 확인 결과)

배포 요청 하나가 만들어질 때 권한은 **두 군데**에 기록된다.

1. **JSON 사이드카** — `$APP_DATA/company-permissions/<owner>_<repo>.json`,
   PR id로 키가 걸린 `PermissionRequestSet`
   ([permissions.go](../../company/permissions.go)의 `SavePermissionRequests`).
   승인 시 실제로 읽히는 건 이 파일이다.
2. **마크다운 요청 기록** — 중앙 저장소의
   `.company/requests/<owner>/<repo>.md`, PR 브랜치 커밋에 포함되어 diff에
   보인다 ([requestlog.go](../../company/requestlog.go)). 지금은 **사람이 읽는
   기록일 뿐** 아무것도 결정하지 않는다. 파일 머리말도 "직접 편집하지
   마세요"라고 적혀 있다.

병합(=승인) 시 `ApplyPermissionsOnMerge`는 JSON을 읽어 **모든 항목에
`Decision = "approve"`를 찍고** apps.yml에 반영한다
([permissions_commit.go:57-63](../../company/permissions_commit.go#L57-L63)).

> 승인은 항목별 결정이 아니다 — 요청자가 낸 것 전부가 승인된 것으로 친다.

즉 **전부 승인 아니면 전부 거절**(PR 닫기)뿐이다. 유일한 부분 선택은 반대
방향으로만 있다: 리뷰 사이드바의 패키지 체크박스는 *요청에 없던* 패키지를
승인에 **더** 얹는다(`GuardDeployApproval` →
[deploy_review.go:226-256](../../company/deploy_review.go#L226-L256)).

요청자 쪽도 마찬가지로 막혀 있다. 한 번 제출한 요청의 항목을 빼거나 사유를
고칠 방법이 없고, 있는 건 `CancelDeployRequest` — 사유를 적고 PR 전체를 닫는
것뿐이다([deployrequests.go:266](../../company/deployrequests.go#L266)).
승인된 권한을 되돌리는 것도 관리자 전용이다(`AdminRevokePackage`,
`AdminSetNetwork`, `AdminSetLimits`). 부서 화면의 권한 목록은 읽기 전용이다
([app_sidebar.go](../../company/app_sidebar.go)의 `permissionRows`).

### 사용자의 전제에 대한 정정

"md 파일을 통해 권한을 수정하고 있다"는 절반만 맞다 — md는 PR diff에 보이는
기록이고, 실제로 적용되는 건 JSON이다. 아래 설계는 그 전제를 **사실로
만드는** 방향이다: md의 체크박스를 결정으로 삼는다.

## 설계 — md의 체크박스가 결정이다

요청 기록의 각 항목을 마크다운 태스크 리스트로 바꾸고, 안정적인 id를 같이
적는다.

```markdown
## 2026-09-25 14:02:11 +0900 — @minji

사내 통계 앱 배포

승인이 필요한 항목:

- [x] `package:pandas` **패키지** — pandas (2.2.0)
  - requirements.txt에 있음
- [x] `network:erp.internal` **외부 접속** — erp.internal (GET)
- [ ] `memory:1024` **메모리** — 512MB → 1024MB

사유: 월말 보고서 마감
```

- 체크된 항목만 apps.yml에 들어간다. 위 예에서 병합하면 pandas와
  erp.internal만 승인되고 메모리 증설은 승인되지 않는다.
- 관리자가 체크를 푸는 것은 **PR 브랜치 위의 커밋**이다. 누가 무엇을 깎았는지
  git이 스스로 답한다 — apps.yml 승인을 커밋으로 남기는 것과 같은 이유다.
- 파일은 PR diff 안에 있으므로, 결정이 코드 변경과 같은 화면에서 보인다.

### 두 파일의 역할 분담 (안전 속성)

| 파일 | 답하는 질문 | 쓰는 주체 |
|---|---|---|
| JSON 사이드카 | **무엇을 요청했는가** — kind/value/label/detail/evidence/reason | 플랫폼 (요청 제출 시) |
| `.company/requests/…md` | **무엇을 승인했는가** — 항목별 체크 | 요청자(철회) / 관리자(선택) |

교집합만 적용한다:

- md에 체크가 있어도 JSON에 없는 항목은 **무시**한다. md에 한 줄 타이핑해서
  권한을 만들어낼 수 없다.
- JSON에 있는데 md에서 찾을 수 없는 항목은 **승인되지 않은 것**으로 본다
  (fail closed).
- 항목 id는 `kind:value` — apps.yml에 실제로 쓰이는 값 그대로라, 사람이 읽는
  라벨이 번역되거나 바뀌어도 결정은 흔들리지 않는다.

### 절차 (관리자)

1. 관리자가 Deploy Request PR을 연다. 사이드바의 "요청된 권한" 패널의 각
   항목에 체크박스가 생긴다(기본값: 전부 체크 — 요청된 그대로).
2. 승인하지 않을 항목의 체크를 푼다.
3. **"승인 항목 저장"**을 누른다 → `POST
   /company/deploy-request/{id}/permissions`가 md의 최신 항목을 다시 써서 PR
   브랜치에 커밋한다. 커밋 작성자는 누른 관리자, 메시지는
   `chore(requests): approve 2 of 3 items for dept/app`. 페이지가 새로고침되고
   diff에 그 변경이 보인다.
4. **"배포 승인"**(병합)을 누른다. `ApplyPermissionsOnMerge`가
   `pr.MergedCommitID`의 md를 읽어 체크된 항목만 apps.yml에 반영한다.
5. 체크가 풀린 항목은 JSON에 `Decision: "reject"`로 남고, PR에 "승인되지 않은
   항목" 코멘트가 한 번 달린다 — 요청자가 물어보지 않아도 알 수 있게.

3번을 건너뛰고 바로 병합하면 요청된 그대로 전부 승인된다 — 지금 동작과 같다.

**저장과 병합을 한 번에 묶지 않는 이유**: 병합 직전에 head 브랜치에 커밋하면
Gitea의 병합 핸들러가 들고 있던 head sha가 낡은 것이 되어 병합이 실패하거나
방금 쓴 커밋을 빠뜨린다. 두 번 누르는 대신 각 누름이 정직하다 — 첫 번째는
PR에 보이는 커밋, 두 번째는 병합.

파일을 직접 고치는 경로도 같은 결과를 낸다. 관리자가 PR의 Files 탭이나
워크스페이스에서 `- [x]`를 `- [ ]`로 바꿔 커밋해도 파싱은 동일하다. 그래서
요청 기록 파일의 머리말에서 "직접 편집하지 마세요"는 **기록 부분에만**
해당하도록 문구를 고친다 — 체크박스는 고치라고 있는 것이다.

### 절차 (요청자, 승인 전)

같은 엔드포인트, 다른 권한 검사. 요청자(또는 관리자)는 PR이 열려 있는 동안:

- 항목을 **뺄** 수 있다 → md에서 그 줄이 사라지고 JSON에서도 빠진다. 커밋
  메시지는 `chore(requests): withdraw 1 item from dept/app`.
- **사유를 고칠** 수 있다.
- 사유 수정은 아직이다 — 사유는 요청 본문에도 들어가 있어서, 한쪽만 고치면
  두 기록이 갈라진다.
- 항목을 **더할 수는 없다.** 항목은 코드 스냅샷에서 탐지되거나 배포 폼에서
  입력된 것이라, 나중에 끼워 넣으면 근거(evidence)가 없는 항목이 생긴다.
  추가가 필요하면 요청을 취소하고 다시 낸다 — 지금도 그 경로다.

권한 검사는 `CancelDeployRequest`와 동일하게 `parseDeployBranchName`의
requesterID 또는 관리자. 닫힌/병합된 PR에서는 거절한다.

좁히는 변경이므로 승인을 기다리지 않는다 — 원칙 그대로다.

### 절차 (부서, 승인 후 반납)

부서 저장소의 앱 화면(`repo_app_panel`)에서 이미 승인된 권한 옆에 "반납"을
둔다. 쓰기 권한이 있는 부서원이면 누를 수 있다.

- 되는 것: 패키지 회수, 외부 접속 호스트 회수, 접근 범위 좁히기, 다운로드
  차단, 메모리·저장공간 한도 **낮추기**.
- 안 되는 것: 넓히는 방향 전부. 핸들러가 방향을 검사해서 거절한다
  (`accessRank` 비교, 한도는 현재값보다 작을 때만). fail closed.
- 이미 있는 뮤테이터를 그대로 쓴다: `withoutPackage`, `removeOutbound`,
  `setAccess`, `setDownload(false)` — 관리자의 직접 변경과 같은 커밋 경로라
  기록이 한 종류다.
- 커밋 작성자는 누른 부서원, 메시지는 `chore(apps): revoke <x> for dept/app`
  + `Handed back by <name>.`
- 누르기 전에 "이 앱이 동작을 멈출 수 있습니다"를 확인 대화상자로 한 번 막는다.

## 확인한 것 (구현 중 검증됨)

- **`files_service.ChangeRepoFiles`는 저장소 쓰기 권한을 검사하지 않는다.**
  검사하는 것은 브랜치 보호뿐이다(`VerifyBranchProtection`,
  `services/repository/files/update.go`). 즉 라우트의 권한 검사가 유일한
  경계이고, 부서원 이름으로 중앙 저장소에 커밋하는 것은 가능하다. 단
  운영자가 중앙 저장소의 기본 브랜치를 보호해 두면 비관리자의 반납 커밋은
  `ErrUserCannotCommit`으로 실패한다 — 그때 화면에 뜨는 것은
  `DepartmentSafeErrorL`을 지난 메시지다.
- **정책이 실행 중인 앱에 닿는 시점은 종류마다 다르다.** 외부 접속은 요청마다
  검사되고(`broker.go`의 `outboundRuleFor(SettingsFor(...))`), 다운로드도
  요청마다다(`proxy.go`). 패키지는 다음 빌드부터다. 그래서 확인 문구도 셋을
  구분해서 말한다.
- **리베이스는 결정을 그대로 들고 간다.** `deploy_rebase.go`가 같은
  `requests` 슬라이스로 새 PR의 JSON과 새 로그 항목을 함께 쓰므로, 체크 상태가
  양쪽에서 일치한 채로 넘어간다.
- **병합 후에는 `pr.MergedCommitID`에서 읽는다.** 브랜치는 그때 이미 지워져
  있으므로(`deploy_notifier.go`), 관리자가 남긴 파일이 남아 있는 곳은 병합
  커밋뿐이다. `readCentralFileAt`이 브랜치 이름과 커밋 id를 모두 받는다.

### 병합 폼에서 체크한 패키지

리뷰 사이드바에는 요청에 없던 패키지를 승인에 얹는 체크박스가 따로 있고,
`GuardDeployApproval`이 병합이 지나가는 동안 그것을 요청에 기록한다. 이 항목은
로그 파일에 없다 — 같은 사람이 같은 화면에서 방금 승인한 것이므로, 파일이
침묵하는 항목이 아니라 이미 결정된 항목으로 취급한다(`decideWithLog`). 그
외에 파일에 없는 항목은 전부 미승인이다.

## 뒤로 호환

이 변경 이전에 제출된 요청의 md 항목에는 체크박스가 없다. **최신 항목에
태스크 리스트가 하나도 없으면** 지금 동작(요청된 전부 승인)으로 떨어진다.
업그레이드 시점에 관리자 앞에 놓여 있던 요청이 갑자기 아무것도 승인하지 않는
요청으로 바뀌면 안 된다.

## 구현 체크리스트

### Phase 1 — md를 결정 기록으로 (완료)
- [x] `requestLogEntry`가 항목을 `- [x] … <!-- perm:kind:value -->` 형태로
      쓴다. id는 렌더링되지 않는 주석에 둔다 — 한국어 줄은 사람이 읽는 것이고
      id는 기계가 읽는 것이라, 한 줄에 둘 다 보이게 할 이유가 없다
- [x] 라벨·상세·근거를 번역해서 쓴다. 필드에 담긴 것은 locale 키이고, 그 키를
      그대로 파일에 써서 결정해야 하는 관리자 앞에
      `company.perm.kind.network`가 놓여 있었다
- [x] `parseRequestLogDecisions(md) (map[string]bool, bool)` — 최신 항목만
      파싱, 두 번째 반환값은 "체크박스가 있는 형식인가"(뒤로 호환 분기)
- [x] `readCentralFileAt`이 브랜치/커밋 ref를 받는다
- [x] `ApplyPermissionsOnMerge`가 `pr.MergedCommitID`의 md로 결정을 정한다
- [x] 단위 테스트: 왕복(쓰기→파싱), 최신 항목만 읽음, 구형 항목 폴백,
      병합 폼 승인 보존, 사라진 줄은 미승인

### Phase 2 — 관리자 선택 승인 UI (완료)
- [x] `POST /company/deploy-request/{id}/permissions`
      (`company/permissions_decide.go`)
- [x] 리뷰 사이드바 "요청된 권한" 패널에 체크박스 + "승인 항목 저장" 버튼
- [x] 최신 항목의 체크만 고치는 최소 diff 재작성(`rewriteRequestLogDecisions`)
      + 같은 내용을 다시 저장하면 빈 커밋을 만들지 않는다
- [x] 미승인 항목을 알리는 PR 코멘트 1회(`sayWhatWasRefused`)
- [x] locale: `company.review.perm_save`, `perm_select_hint`, `perm_saved`,
      `perm_refused`
- [ ] AI 리뷰 컨텍스트가 "요청됨"과 "승인하기로 한 것"을 구분해서 말한다
      (지금은 요청 전체를 넣는다 — 필요해지면)

### Phase 3 — 요청자의 철회/수정 (일부 완료)
- [x] 같은 엔드포인트의 요청자 경로 — 체크를 푼 항목은 기록에서 사라진다.
      관리자의 미승인과 달리 남기지 않는다: 아무도 결정하지 않은 것에 대해
      남길 결정이 없다
- [x] 부서 대시보드(`deploy_requests.tmpl`)의 열린 요청마다 항목 목록 +
      "요청 항목 수정"
- [x] 리다이렉트는 각자 온 곳으로. 부서원은 중앙 저장소 PR을 열 수 없으므로
      부서 목록으로 돌아가고, 부서 이름은 폼 값이 아니라 브랜치에서 읽는다
- [ ] 사유 수정 — 항목 제거만 먼저 넣었다. 사유는 요청 본문에도 들어가 있어서
      한쪽만 고치면 두 기록이 갈라진다

### Phase 4 — 승인된 권한 반납 (완료)
- [x] `POST /{owner}/{repo}/_app/permissions/revoke`
      (`company/permissions_revoke.go`, `reqRepoCodeWriter` 뒤)
- [x] 넓히는 방향은 **경로 자체가 없다.** 핸들러가 부를 수 있는 것은
      `RevokeAppPackage`/`removeOutbound`/`setDownload(false)` 셋뿐이라,
      방향 검사를 런타임에 하는 것보다 좁다
- [x] 앱 화면의 권한 표에 "반납" 버튼, 승인된 패키지 라벨마다 ×
- [x] 확인 문구가 종류별로 다르다 — 패키지는 "다음 빌드부터", 나머지는 즉시
- [x] locale: `company.app.revoke*`, `company.flash.perm_revoked`
- [ ] 메모리·저장공간 한도 낮추기는 넣지 않았다. 한도를 스스로 낮추는 것은
      권한을 반납하는 것이 아니라 앱이 더 빨리 죽게 만드는 것이고, 그걸 원하는
      부서는 없다. 필요해지면 같은 경로에 얹으면 된다
