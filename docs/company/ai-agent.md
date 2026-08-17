# AI Agent (사내 AI 툴 연동)

이 문서는 Gitea 웹 UI 안에 "Claude Code 같은" AI 에이전트를 붙이는 작업의
설계/체크리스트 문서다. 구현하면서 이 파일의 체크박스를 그때그때 갱신한다 —
새 세션에서 이어받을 때도 이 파일만 보면 뭐가 됐고 뭐가 안 됐는지 알 수 있게.

## 왜 "그냥 API 한 번 호출"로는 안 되는가

사용자가 지적한 게 맞다 — Claude Code가 유용한 이유는 모델 자체가 똑똑해서가
아니라, 모델 주변에 있는 **에이전트 루프 + 도구 + 상태 관리**가 잘 되어 있어서다.
채팅 API를 한 번 부르고 텍스트를 돌려주는 건 챗봇이지 에이전트가 아니다.
에이전트가 되려면 최소한 아래가 다 있어야 한다:

1. **도구(tool)를 여러 번, 스스로 판단해서 호출** — 파일을 읽고, 그 내용을 보고
   판단해서 또 다른 파일을 읽거나 수정하는 걸 모델 스스로 반복 (이미
   `company/workspace_ai.go`에 루프는 있음 — 문제는 아직 얕다, 아래 참고)
2. **대화가 이어져야** — "이것도 고쳐줘" 같은 후속 요청이 이전 맥락(뭘 읽었는지,
   뭘 고쳤는지)을 기억해야 함
3. **진행 상황이 실시간으로 보여야** — 뭘 하고 있는지 (파일 읽는 중.../
   `src/util.py` 수정 제안함) 사람이 보면서 신뢰할 수 있어야
4. **안전 경계가 명확해야** — 특히 이 시스템은 "사람이 검토 후 저장" 원칙 위에
   지어져 있으므로, 에이전트가 이 원칙을 절대 건너뛰면 안 됨
5. **실패를 우아하게 처리** — API 에러, 타임아웃, 무한 루프 방지

## 이 환경에서 안 되는 것 (정직하게)

- **코드 실행/테스트 실행**: 이 Gitea 인스턴스에는 샌드박스 실행기가 없다.
  Claude Code처럼 "테스트를 돌려보고 결과를 보고 다시 고치는" 루프는 지금
  구조로는 불가능 — 파일을 읽고/쓰는 것까지만 가능. (나중에 Gitea Actions
  러너를 연결하면 "커밋 후 CI 결과를 다시 읽어오기" 정도는 흉내낼 수 있지만,
  이건 완전히 별도 작업.)
- **레포 전체를 한 번에 다 읽어서 맥락에 넣기**: 토큰 제한 때문에 불가능.
  모델이 필요한 파일만 골라서 읽도록 도구로 유도해야 함 (이미 그 방향으로
  설계됨 — list_files로 목록만 먼저 보고, 필요한 것만 read_file).
- **여러 사용자의 대화를 서버에 영구 저장**: 지금은 프론트엔드(브라우저)가
  대화 기록을 들고 있다가 매 요청마다 같이 보내는 방식 — DB에 대화 테이블을
  새로 만들지 않기 위한 선택. 새로고침하면 대화가 날아간다는 뜻이기도 함
  (아래 "미해결 질문" 참고).

## 워크스페이스 에디터: 저장 안 된 변경사항은 지속시키지 않는다

한때 새로고침/크래시에도 타이핑한 내용이 안 날아가도록 `localStorage`에
초안(draft)을 자동 저장했다가, 같은 파일을 다시 열면 복구해주는 기능이
있었다 — 그런데 이게 실제로는 크래시 방지보다 더 자주, 더 조용히 데이터를
꼬이게 만드는 원인이 됐다: 오래된 초안이 방금 저장에 성공한(또는 AI가 막
생성한) 최신 내용을 덮어써 버리는 사고가 반복됐다 — 서버(레포)에는 맞는
내용이 있는데 에디터 화면에만 다른 내용이 보이는, 진단하기 까다로운 버그.

그래서 이 기능은 완전히 제거했다: 페이지를 벗어나면(`pagehide`) 남아 있는
게 없고, 파일을 열 때마다 항상 서버의 최신 내용을 새로 가져온다. 크래시
방지라는 장점보다 "화면에 보이는 게 실제로 저장될 내용과 다를 수 있다"는
위험이 훨씬 크다고 판단 — `company-workspace.ts`의 `clearLegacyDrafts` 참고.

## 안전 경계 (타협 불가)

- **에이전트는 git에 직접 쓰지 않는다.** `write_file` 도구는 제안(proposal)만
  만들고, 실제 커밋은 사람이 에디터에서 "저장" 버튼을 눌러야만 일어난다 —
  이 시스템 전체의 원칙(`docs/company/architecture.md`)과 동일.
- **에이전트는 현재 열려 있는 repo+branch 밖을 절대 못 건드린다.** 도구는
  항상 `ctx.Repo.Repository`/`ctx.Repo.BranchName`에 하드 스코프됨 — 모델이
  다른 레포 이름을 지어내도 도구 구현 자체가 그 레포를 열 방법이 없음.
- **API 키는 유저별로 분리.** 공용 키 없음 — 서로 다른 직원이 서로 다른 키/
  모델/추론 강도를 쓸 수 있고, 한 사람 키가 새도 다른 사람에게 영향 없음.
- **AI 실패는 절대 핵심 워크플로우(저장, Deploy Request 제출)를 막지 않는다.**
  Deploy Request 리뷰 코멘트가 실패해도 요청 제출 자체는 성공해야 함 — 이미
  `postAIReviewComment`가 이렇게 되어 있음 (에러는 로그만, 응답엔 영향 없음).
- **무한 루프 방지.** 한 요청당 도구 호출 턴 수 상한 (`workspaceAIMaxTurns`).

## 등장 위치 (3곳)

| 위치 | 목적 | 도구 | 상태 |
|---|---|---|---|
| 워크스페이스 에디터 우측 사이드바 (`/{owner}/{repo}/_edits/{branch}`) | 코드 생성/수정 도움 | list_files, read_file, write_file | 진행 중 |
| PR 화면 좌측 사이드바 (`{owner}/{repo}/pulls/{index}`, 특히 central-deploy) | 리뷰 중인 admin이 diff에 대해 질문 | read_file(base/head), get_diff, list_changed_files (읽기 전용) | 미착수 |
| Deploy Request 제출 시 자동 리뷰 코멘트 | 사람이 보기 전에 1차 요약/이상 징후 알림 | 없음 (diff 텍스트를 통째로 프롬프트에 넣는 단발성 호출) | 완료 |

## 아키텍처 결정 (이미 확정, 재논의 안 함)

- **OpenAI 호환 chat completions + function calling** — 사내 AI 툴이 이 형식.
- **스트리밍은 POST + 개행 구분 JSON(ndjson) 바디** — `EventSource`는 GET만
  지원해서 안 됨. `fetch()` + `ReadableStream`으로 직접 파싱.
- **게이트웨이 URL은 인스턴스 전역** (`app.ini`의 `[company] AI_API_URL`),
  **모델/키/추론강도는 유저별** (`/user/settings/ai`, `company/settings_ai.go`,
  `user_model.GetUserSetting`/`SetUserSetting` 재사용 — 스키마 변경 없음).
- **대화 기록은 프론트엔드가 들고 있다가 매 요청에 같이 보냄** — 서버는
  요청 하나 처리하고 끝, 세션 상태를 서버에 두지 않음.
- **백엔드 공통 클라이언트**: `company/ai.go` (`aiChatTurn`, `aiChatTurnStream`) —
  워크스페이스/PR리뷰 두 사이드바가 같은 클라이언트를 쓰고, 도구 목록(tool set)과
  시스템 프롬프트만 위치별로 다름. OpenAI 호환/Anthropic 두 provider를 다
  지원 (`/user/settings/ai`에서 유저가 선택 — Anthropic은 게이트웨이 URL 없이
  본인 키로 Anthropic API에 직접 연결).
- **도구는 자체 스키마가 아니라 실제 MCP(Model Context Protocol) 서버로 정의**
  (`company/mcp.go`, `github.com/modelcontextprotocol/go-sdk`) — 이전에는
  `aiTool` 구조체를 손으로 만들어 썼는데, 이제 list_files/read_file/write_file이
  진짜 MCP 서버로 등록되고, 같은 프로세스 안에서 `mcp.NewInMemoryTransports`로
  붙은 MCP 클라이언트가 이걸 호출한다 (진짜 소켓/서브프로세스 아님 — in-memory
  전송이지만 프로토콜 자체는 표준). 요청마다 서버+세션을 새로 만들어 그
  요청의 `edits`/`openFiles` 상태에 클로저로 묶는다. 도구 목록은 매 요청
  `session.ListTools()`로 가져와서 provider별 스키마로 변환
  (`mcpToolsToAI`) — 더 이상 `workspaceAITools` 같은 고정 변수가 없다.
  장점: (1) 표준 프로토콜이라 나중에 외부 MCP 서버(공식 filesystem 서버 등)를
  섞어 쓰기 쉬움, (2) 이 3개 도구 자체도 나중에 다른 MCP 클라이언트가 재사용
  가능. `mcpResultToText`가 `CallToolResult`를 우리 에이전트 루프가 기대하는
  평문 문자열로 변환한다.

## 구현 체크리스트

### Phase 1 — 기반 (완료)
- [x] 유저별 AI 설정 페이지 (`/user/settings/ai`: 모델 ID/API 키/추론강도)
- [x] 인스턴스 전역 게이트웨이 URL (`app.ini`)
- [x] OpenAI 호환 chat client + function calling 지원 (`company/ai.go`)
- [x] Deploy Request 자동 리뷰 코멘트 (단발성, non-streaming)
- [x] 설정 미완료 시 모든 기능이 조용히 비활성화 (`AIConfiguredFor`)

### Phase 2 — 워크스페이스 에디터 사이드바
- [x] 백엔드 에이전트 루프 (읽기/쓰기 도구, ndjson 스트리밍) — `WorkspaceAI`,
      `aiChatTurnStream` (`company/ai.go`)
- [x] 우측 채팅 사이드바 UI (메시지 목록 + 하단 입력창, Claude Code VS Code
      확장 스타일) — `company-ai-chat.ts` + `workspace.tmpl`
- [x] 제안된 수정사항을 열린 탭에 실시간 반영 — `applyAIEdit`
      (`company-workspace.ts`), 상태 줄에 "✅ path 수정 제안됨" 표시. 저장
      전까지는 일반 미저장 변경과 동일하게(탭 dirty 표시) 취급 — 별도
      "AI 제안됨" 배지는 아직 없음, 필요해지면 추가
- [x] 프론트엔드가 대화 기록(user/assistant 텍스트만)을 들고 있다가 다음
      요청에 `history`로 같이 보냄 → 후속 요청 맥락 유지
- [x] 에러 상태를 채팅창 안에 표시 (실패 시 말풍선에 에러 메시지)
- [x] 진행 중 취소 — 전송 버튼이 전송 중엔 "중단"으로 바뀌어 `AbortController`로
      중단
- [x] 실제 게이트웨이 없이도 전체 파이프라인(설정→키 조회→스트리밍 호출→
      에러 스트림) 동작 확인 — fake URL로 connection-refused까지 왕복시켜
      `{"type":"error",...}`가 정상적으로 클라이언트에 도착하는 것까지 검증함.
      실제 성공 응답(텍스트/도구 호출 파싱)은 진짜 게이트웨이가 있어야 검증 가능
- [ ] "AI 제안됨" 탭 배지 등 시각적 구분 강화 (선택, 필요해지면)
- [ ] 대화 기록을 `localStorage`에 저장해서 새로고침에도 유지할지 — 미해결
      질문 참고, 사용자 확인 필요

### Phase 3 — PR 리뷰 사이드바
- [ ] 읽기 전용 도구셋: `read_file(side, path)` (base/head 어느 쪽인지),
      `list_changed_files()`, `get_diff()` — 이미 `deployrequestfiles.go`/
      `gitdiff.GetDiffForRender`에 있는 diff 계산 로직 재사용
- [ ] `templates/repo/issue/view.tmpl` (또는 그 하위 partial) 커스텀 오버라이드로
      좌측 사이드바 추가 — 네이티브 PR 화면을 깨지 않게 조심
- [ ] 같은 스트리밍 프로토콜/프론트엔드 모듈 재사용 (Phase 2와 코드 공유)
- [ ] 어떤 PR에서든(Deploy Request 전용 아님) 동작하게 할지, central-deploy
      PR로 한정할지 결정 필요 — 현재는 후자로 가정하고 있음 (아래 미해결 질문)

### Phase 4 — 다듬기
- [ ] 로딩/에러 상태 UI 통일
- [ ] (선택) admin이 팀 전체 AI 사용량을 볼 수 있는 화면 — 지금 범위 밖,
      필요해지면 별도 착수

## 프론트엔드 공유 설계

워크스페이스와 PR 사이드바가 채팅 UI 자체는 거의 동일해야 하므로, 공통 모듈로
분리한다 (아직 미작성):

- `web_src/js/features/company-ai-chat.ts` — 메시지 렌더링, ndjson 스트림
  파싱, 입력창/전송/취소 — 도구 실행 결과 표시 방식은 콜백으로 위임
- `company-workspace.ts`가 이 모듈을 가져다 쓰고, `edit` 이벤트를 받으면
  열린 탭에 반영
- (Phase 3에서 만들 예정) `company-pr-chat.ts`는 같은 모듈을 가져다 쓰고,
  `edit` 이벤트는 무시 (읽기 전용이므로 애초에 안 옴)

## 백엔드 스트리밍 프로토콜 (ndjson, 한 줄에 JSON 객체 하나)

```
{"type":"text","delta":"이 파일은..."}       // 텍스트 조각, 이어붙이기
{"type":"tool","name":"read_file","args":{"path":"src/util.py"}}  // 상태 표시용
{"type":"edit","path":"src/util.py","content":"..."}  // 제안된 파일 전체 내용
{"type":"done"}
{"type":"error","message":"..."}
```

## 미해결 질문 (사용자 확인 필요, 임의로 정하지 않음)

1. **대화 기록을 새로고침해도 유지할지?** 지금 계획은 "브라우저 메모리만,
   새로고침하면 날아감"인데, 실제로 편집이 오래 걸리는 작업이면 불편할 수
   있음 — 필요하면 `localStorage`에 저장하는 정도는 쉽게 추가 가능 (draft
   저장과 같은 패턴).
2. **PR 사이드바를 모든 PR에 넣을지, central-deploy만?** 부서 레포에도
   일반 PR이 있을 수 있는가? (지금 구조에서는 부서 레포는 direct commit만
   쓰고 PR을 안 씀 — 그렇다면 PR 사이드바는 사실상 central-deploy 전용이 됨.)
3. **비용/사용량 가시성이 필요한지?** 유저별 키를 쓰므로 사용량은 각자 키
   발급처(사내 AI 툴 관리 화면)에서 보일 텐데, Gitea 안에서도 보여줄 필요가
   있는지.
