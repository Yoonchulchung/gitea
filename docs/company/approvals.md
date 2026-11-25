# 승인 — 배포와 프로젝트를 한 곳에서

관리자가 결정해야 하는 것은 모두 `company_approval_request` 테이블 한 곳에 있고
(`company/approvals.go`), 화면은 `/company/approvals` 하나다
(`company/approvals_page.go`, 관리자만). 상단 메뉴의 "승인"에 대기 건수가 붙고,
관리자 대시보드 맨 위에 대기 중인 요청이 보인다.

| 종류 | 행이 생기는 때 | 결정하는 곳 |
|---|---|---|
| `project` | 새 프로젝트 요청 (`RequestProject`) | 승인 페이지의 승인·거절 버튼. 승인하면 요청자를 소유자로 프로젝트를 만든다 |
| `deploy` | 중앙 저장소에 배포 요청 PR이 열릴 때 (`openPullRequest`) | PR 검토 페이지 — 권한 항목별 결정과 AI 검토가 거기 있다 |

배포 행은 PR을 따라간다: merge면 승인, 부서가 취소하면(`cancelCommentPrefix` 댓글)
취소, 그 밖의 닫힘은 거절, 다시 열면 대기. 알림 훅(`company/deploy_notifier.go`)이
바로 반영하고, 시작할 때 `SyncDeployApprovals`가 중앙 저장소의 모든 배포 PR로 표를
채우고 놓친 상태를 바로잡는다.

결정은 대기 중인 행에 대한 조건부 UPDATE라, 두 관리자가 동시에 눌러도 한 번만 결정된다.

중앙 저장소를 걷어낼 때는 배포 행이 PR을 가리키는 `RefID`와 위 동기화만 바꾸면 된다 —
화면과 대기 건수는 이 표만 본다.
