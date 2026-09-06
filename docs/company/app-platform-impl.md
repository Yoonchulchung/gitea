# 앱 배포 플랫폼 — 구현 계약

**코드를 쓰기 전에 읽는 문서.** 아키텍처와 근거는 [app-platform.md](app-platform.md), 진행은 [app-platform-tasks.md](app-platform-tasks.md).

여기 있는 규칙은 제안이 아니라 계약이다. 어기면 다중 사용자 환경에서 경쟁 조건·성능 저하·보안 사고가 난다.

## 1. 파일 배치

전부 `AppDataPath` 아래 — 쓰기 권한이 보장된 유일한 곳.

```
<AppDataPath>/company-apps/<hash>/          # 앱 하나의 홈
  releases/<sha>/{app/, .venv/}             # 릴리스별 소스+venv (롤백이 의존성까지 되돌린다)
  current -> releases/<sha>                  # 원자적 전환점
  previous -> releases/<sha>
  run/app.sock                               # bwrap에 바인드되는 유일한 rw 지점
  logs/app.log                               # 회전: 10MB × 5
<AppDataPath>/company-app-state/<hash>.json  # 상태 (desired/actual/이력)
<AppDataPath>/company-metrics/<hash>.json    # 5분 버킷 30일
<AppDataPath>/company-env/<hash>.json        # 환경변수 (암호화)
```

`<hash>` = `sha256(owner + "/" + repo)`. `-`·`.`이 org·repo 양쪽에 합법이라(`modules/validation/helpers.go:28`) 평면 조합은 실제로 충돌한다(`a-b/c` vs `a/b-c`). owner/repo는 JSON 안에 저장해 "전체 목록"이 디렉터리 순회로 끝나게 한다. 같은 이유로 같은 방식을 쓰는 선례: `company/workspace_tmp.go:119`.

권한: 디렉터리 `0700`, 파일 `0600` (`company/deploy_snapshot.go` 선례).

## 2. 동시성

### 재사용할 기존 패턴 — 새로 만들지 않는다

- **per-key 뮤텍스**: [`workspaceTmpLockFor`](../../company/workspace_tmp.go) (`workspace_tmp.go:102-111`) 방식. 맵 접근만 전역 락, 키별 뮤텍스 반환
- **원자적 파일 쓰기**: temp 파일 + `os.Rename` (`workspace_tmp.go:157-160`). 같은 파일시스템에서 원자적이라 독자가 반쯤 쓰인 파일을 볼 수 없다

### 앱별 상태 머신 — 모든 전이의 단일 통로

배포·시작·중지·재시작·watchdog kill·관리자 정지·재조정이 **전부 같은 앱 뮤텍스 안에서** 일어난다. 별도 경로를 만들면 "재시작 중에 stop이 끼어드는" 류의 경쟁이 생긴다.

```go
type appSupervisor struct {
    mu      sync.Mutex   // 상태 전이 전체를 감싼다
    key     string       // hash(owner/repo)
    cmd     *exec.Cmd    // bwrap 프로세스 (nil = 안 떠 있음)
    state   appState     // actual
    desired appState
}
```

전이 규칙:

| 현재 | 허용되는 전이 | 거부 |
|---|---|---|
| `stopped` | start(부서·관리자), deploy | — |
| `running` | stop, restart, deploy(=신규 릴리스로 교체), suspend | — |
| `suspended` | resume(**관리자만**), remove | 부서의 start |
| `failed` | start, deploy, remove | — |
| 배포 진행 중 | (뮤텍스가 잡혀 있어 자연히 직렬화) | 동시 배포 |

`desired_state`는 부서·관리자의 의도(`running`/`stopped`), `actual_state`는 실제(`running`/`stopped`/`failed`/`suspended`). **Gitea 기동 시 재조정은 1회만**: 상태 파일을 훑어 `desired=running`인 앱을 띄운다. 소켓 파일이 남아 있으면 **PID가 살아있는지 확인한 후에만** 삭제하고 기동한다(stale 소켓 bind 실패 방지).

### apps.yml 동시 승인

두 관리자가 동시에 승인 버튼을 누르면 커밋이 충돌한다. **낙관적 재시도**: 커밋 직전 최신 내용을 다시 읽고 병합해 커밋, 실패 시 최대 3회 재시도, 그래도 실패면 "다시 시도하세요" 오류. `files_service.ChangeRepoFiles`(`company/deploy.go:623`이 쓰는 것)를 재사용한다.

### 검증

동시성 코드는 전부 `go test -race`로 돈다. 최소: 같은 앱에 병렬 deploy+stop+restart를 퍼붓는 테스트, 지표 카운터 병렬 증가 테스트.

## 3. 성능 — 프록시 핫패스가 성역이다

프록시는 **모든 앱 요청의 경로**다. 여기가 느려지면 플랫폼 전체가 느려진다.

### 요청당 허용되는 일 (이것뿐)

1. 인메모리 앱 레지스트리 조회 (RWMutex 읽기 락 또는 `atomic.Pointer` 스왑 방식)
2. 접근 모드 검사 (메모리 값 비교)
3. `sync/atomic` 지표 카운터 증가
4. 유닉스 소켓으로 프록시 전달

### 요청당 금지

- ❌ **파일 읽기** — 앱 목록·정책은 시작 시 로드하고 변경 시(승인 커밋 후) 무효화
- ❌ **뮤텍스** — 지표는 atomic. 경합이 보이면 앱별 구조체로 샤딩
- ❌ **파일 쓰기** — 버킷 flush는 백그라운드 goroutine이 30초 주기로. flush 실패는 로그만 남기고 다음 주기에 재시도(지표 손실 < 요청 지연)

### 요청 goroutine에서 절대 하면 안 되는 것

- **`pip install`** (수 분) — merge 훅(`deploy_notifier.go`의 `MergePullRequest`)은 **작업을 큐에 넣고 즉시 반환**한다. 훅을 오래 잡으면 merge 자체가 느려진다
  - 배포 워커: 전역 워커 N개(기본 2), 같은 앱은 앱 뮤텍스로 직렬화
  - 큐 깊이 상한(기본 32), 초과 시 상태를 `failed("deploy queue full")`로 — 무한 적체 방지
- venv 생성, 디스크 용량 계산(du), 로그 스캔

### 기타

- 로그 검색: **스트리밍 읽기**(`bufio.Scanner`, 전체 로드 금지) + 컨텍스트 타임아웃(10초) + 결과 상한(1000줄)
- 대시보드: 요청된 기간의 버킷만 파싱, 화면 폭 이상은 다운샘플링
- `/proc` 샘플링: watchdog과 같은 goroutine에서 5~10초 주기. 앱 수에 O(n)이므로 100앱 규모까지는 문제없으나 그 이상이면 주기 조정

## 4. 예외 처리 — fail-open vs fail-closed

기존 원칙의 확장이다: "AI 실패는 절대 핵심 워크플로우를 막지 않는다"([ai-agent.md](ai-agent.md)).

**규칙: 관측 실패는 fail-open, 보안 실패는 fail-closed.**

| 실패 | 처리 |
|---|---|
| 지표 기록·버킷 flush 실패 | **open** — 로그만, 요청 정상 처리 |
| 로그 파일 쓰기 실패 | **open** — 앱 계속 실행 |
| 상태 파일 손상(JSON 파싱 실패) | **open** — 기본값으로 재생성, 앱 죽이지 않음, 관리자 화면에 경고 |
| `apps.yml` 파싱 실패 | **open** — **마지막 정상 설정 유지**, 관리자 화면에 오류. 깨진 YAML이 돌던 앱을 죽이면 안 된다 |
| 시크릿 복호화 실패 | **closed** — 기동 거부 + 원인 코드. 빈 값 진행은 추적 지옥 |
| 샌드박스 적용 불가(bwrap 없음·userns 차단) | **closed** — 기동 거부. 관리자 명시 옵트인으로만 예외 |
| 패키지 미승인·requirements 검증 실패 | **closed** — 배포 중단, **이전 버전 유지**(교체 전이므로 무중단) |
| 헬스체크 실패 | 자동 롤백 → 복원본 재검사 → 복원본도 실패면 `failed`로 보고(`rolled_back`과 구분 — "되돌아갔다"와 "지금 죽어있다"는 다르다) |

모든 실패는 **원인 코드**로 분류해 상태 파일에 남긴다:
`install_failed` · `package_denied` · `oom` · `health_timeout` · `crash_loop` · `suspended` · `sandbox_unavailable` · `secret_error` · `deploy_queue_full`

부서에는 평문 요약 + 조치 버튼(예: `package_denied` → [승인 요청]), 관리자에게는 원문 로그.

## 5. 보안 규칙 — 알고리즘으로 사고 내지 않기

| 위험 | 규칙 |
|---|---|
| 경로 주입 | 사용자 입력을 경로에 넣지 않는다 — 키는 sha256. 불가피하면 `filepath.Join` 후 `strings.HasPrefix`로 루트 접두사 검증 |
| 임의 소켓 접근 | 프록시는 `{org}/{repo}`를 **인메모리 앱 레지스트리와 대조** 후에만 연결 |
| 타이밍 공격 | 토큰 비교는 `subtle.ConstantTimeCompare` |
| **ReDoS** | 로그 검색은 **부분 문자열 매칭이 기본**. 정규식을 열려면 컴파일 타임아웃 + 실행 컨텍스트 타임아웃 필수 |
| XSS | 로그·앱 이름·설명 등 직원 입력은 렌더링 시 이스케이프. `innerHTML` 금지 |
| 환경변수 주입 | 이름 검증: `LD_*`·`PATH`·`HOME`·`PYTHON*`·`APP_SOCKET`·`ROOT_PATH` 거부. `^[A-Z][A-Z0-9_]*$`만 허용 |
| 부모 환경 유출 | `exec.Command`에 **`cmd.Env`를 반드시 명시** — 미지정 시 Gitea의 환경(DB 비밀번호 등)이 통째로 상속된다. 허용 목록: `PATH`·`HOME`·`LANG`·플랫폼 변수·앱 변수 |
| argv 시크릿 노출 | `bwrap --setenv` 금지(ps에 보인다). bwrap 프로세스의 `cmd.Env`로 넘겨 상속시킨다 |
| requirements 주입 | `^[A-Za-z0-9][A-Za-z0-9._-]*==[A-Za-z0-9.!+*_-]+$` 형태만. `git+`·`http`·`-e`·`-r`·`--index-url`·`--extra-index-url` 거부 |
| 시크릿 로그 유출 | 빌드 로그·오류 메시지에 환경변수 값을 넣지 않는다. 오류에 값이 섞일 수 있는 지점(pip 출력 등)은 알려진 시크릿 값으로 마스킹 |

## 6. bwrap 기동 커맨드 (기준형)

```
bwrap --unshare-all --die-with-parent --new-session \
      --proc /proc --dev /dev \
      --tmpfs /tmp --size <tmp상한> /tmp \
      --ro-bind /usr /usr --ro-bind /lib /lib [--ro-bind /lib64 /lib64] \
      --ro-bind <release>/app /app --ro-bind <release>/.venv /venv \
      --bind <apphome>/run /run \
      --chdir /app \
      -- /venv/bin/uvicorn main:app --uds /run/app.sock --root-path /apps/<org>/<repo>
```

- 환경은 전부 `cmd.Env`로 (5절)
- 기동 직전 자식에서 `rlimit` 설정: `RLIMIT_DATA`(메모리 — `RLIMIT_AS`는 numpy 가상 예약과 충돌 가능, 4단계에서 실측 비교), `RLIMIT_NPROC`, `RLIMIT_NOFILE`, `RLIMIT_FSIZE`
- `nice` 상향(Gitea보다 낮은 우선순위), `oom_score_adj` 상향(OOM 시 앱 먼저)
- `Setpgid` + bwrap이 PID 네임스페이스 PID 1이므로 **bwrap만 죽이면 전체 정리**

## 7. 헬스체크

1. `curl --unix-socket` `GET /health` → 404면 `GET /` 폴백. **5xx 아니고 연결되면 성공**
2. 1초 간격 최대 30회. 사이사이 프로세스 생존 확인 — 죽었으면 즉시 중단
3. 첫 성공 후 **2초 간격 3회 연속** 성공 요구 (한 번 응답하고 죽는 앱)
4. 10초 후 재시작 카운트 비교 — 증가했으면 **크래시 루프, HTTP 통과해도 실패**
5. 실패 시: `current`→`previous` 원자 복원 → 재기동 → 같은 검사 → 복원본도 실패면 `failed`

## 8. 테스트

[AGENTS.md](../../AGENTS.md) 규칙: 가장 적고 빠른 테스트, 기존 확장 우선, 단위 우선.

**필수 (로직 고립 가능 + 틀리면 보안 사고):**
- requirements 파싱: 정상/`git+`/URL/`-e`/`--index-url`/주석·공백 — `company/deploy_test.go` 확장
- 환경변수 이름 검증: 정상/`LD_PRELOAD`/`PATH`/소문자
- 경로 키: 해시 충돌 회피(`a-b/c` vs `a/b-c`), 접두사 검증
- 스냅샷 delete(선행 버그): 삭제만/최초 배포/리네임/파일↔디렉터리
- 상태 머신: 전 전이표, `suspended`에 대한 부서 start 거부, desired 존중
- 버킷 집계: 평균·최대·p95, 빈 버킷
- 프록시 정책: 접근 모드 3종, 미등록 앱 404, `X-Forwarded-User` 스트립, `Content-Disposition` 차단, 크기 상한

**`-race` 필수:** 동시 배포+제어, 지표 병렬 증가, 레지스트리 리로드 중 요청.

**통합(느려서 최소한만):** 실제 토이 앱 배포→헬스→프록시 응답, 깨진 앱 롤백. `AGENTS.md`의 sub-2s 기준을 지키기 어려우면 빌드 태그로 분리.

## 정보 노출 경계 (2026-09-06 추가)

부서 화면에 서버 절대 경로가 그대로 뜨는 버그를 계기로, **비관리자에게 무엇이
닿는가**를 전수 감사하고 구조로 고정했다.

원인은 하나의 실수가 아니라 Go의 기본 동작이다. os 계열 오류는 전부
`*fs.PathError`라 **실패한 절대 경로를 품고 있다.** `os.Readlink(current)` 하나가
`readlink /home/git/gitea/data/company-apps/9646…/current: no such file` 이 되고,
그걸 `err.Error()`로 흘리면 샌드박스가 숨기려는 바로 그 디렉터리 위치를 알려준다.

**원칙: 가림(redaction)이 아니라 허용(opt-in).** 가림은 새는 모양을 전부
열거해야 하고 하나 놓치면 끝난다. 대신 **부서용으로 일부러 쓴 문장만** 통과시키고
나머지는 전부 일반 문구 + 관리자용 로그로 보낸다.

| 장치 | 역할 |
|---|---|
| `userError` (`company/usererror.go`) | 부서용으로 쓴 메시지에만 붙이는 타입 |
| `DepartmentSafeError(ctx, err)` | 안전한 것만 반환, 나머지는 로그로 보내고 일반 문구 |
| `AppState.UserMessage` | `Message`(관리자 전용)와 **필드 자체를 분리** |
| `RedactServerPaths` | pip 같은 외부 도구 출력용 이중 안전장치 |

`DepartmentCause`는 `st.Message`를 **한 번도 읽지 않는다.** 미분류 원인 코드는
`UserMessage`가 비어 있으므로 요약 문장만 나간다 — 조용히 새는 것보다 낫다.

감사에서 나온 노출 지점 4곳(전부 수정):
`AppControl` 플래시, `AppEnvSave` 플래시, `AppPage`의 `EnvError`,
`DepartmentCause`의 `ReasonContractViolation` 통과. 관리자 화면
(`admin_app.go`)은 원문 그대로가 맞고, 그 자리에 의도임을 명시했다.
