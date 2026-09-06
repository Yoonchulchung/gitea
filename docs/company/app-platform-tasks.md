# 앱 배포 플랫폼 — 구현 체크리스트

각 항목은 독립적으로 검증 가능해야 하며, 완료 시 `[x]`로 바꾼다.
설계 근거는 [app-platform.md](app-platform.md), 구현 규칙은 [app-platform-impl.md](app-platform-impl.md).

되돌리기 어려운 것이 뒤로 가도록 배치했다. **3단계까지는 macOS에서 개발·검증 가능하다**(systemd를 안 쓰므로 dev/prod 분기가 없다).

## 0. 선결 확인 (서버에서, 코드 작성 전)

- [x] `apt-cache policy bubblewrap` 설치 가능 — **확인됨** (2026-09-06)
- [x] ~~`unshare -Ur true` 성공~~ — **실패함 (2026-09-06):** `write failed /proc/self/uid_map: Operation not permitted`.
  비특권 user namespace가 커널에서 차단되어 있다. bubblewrap은 설치되지만 샌드박스를 만들지 못한다.
  **이 설계의 격리 전제가 무너진 상태이며, 해결 전까지 앱은 기동되지 않는다**(fail-closed, 의도된 동작).
  설치 여부만 보고 통과시켰다면 운영에 올린 뒤에야 알았을 문제다. `sandboxProbe`
  (company/appsandbox.go)가 `LookPath`로 끝내지 않고 실제로 네임스페이스를
  만들어 보게 짠 것이 이걸 기동 시점에 잡아준다.
  → 아래 **0-A. 샌드박스 대안 결정**
- [x] ~~`prlimit --version`~~ — **불필요해짐.** Landlock 경로에서는 헬퍼가 자기 자신에게 `setrlimit`을 건다. bubblewrap 경로에서만 쓰이며 없어도 fail-open이다
- [x] PyPI 접근 — **가능함 (2026-09-06)**. 막혀 있었다면 `pip install`이 전부 실패해
  빌드 단계가 통째로 성립하지 않았을 것이므로, 남아 있던 위험 중 가장 컸다
- [ ] `uname -r` 기록 (커널 버전 — user namespace 관련)

## 0-A. 샌드박스 대안 결정 (해결됨 — Landlock + seccomp)

비특권 user namespace가 막혀 있어 bubblewrap을 쓸 수 없다. 아래 중 하나를
택하기 전까지 앱을 운영에 올려서는 안 된다. 격리 없이 도는 앱은 Gitea와 같은
계정이므로 `data/sessions/`를 읽어 **관리자로 즉시 로그인**할 수 있다.

레버리지 순서 (서버 관리자에게 요청하는 쪽이 압도적으로 싸다):

- [ ] **(A) userns 허용 요청** — 배포판에 따라 한 줄이다.
      Ubuntu 24.04+: `kernel.apparmor_restrict_unprivileged_userns=0`,
      Debian: `kernel.unprivileged_userns_clone=1`,
      RHEL 계열: `user.max_user_namespaces` > 0.
      **지금까지 만든 것이 수정 없이 그대로 동작한다.**
      정직한 반대급부: 비특권 userns는 커널 공격면을 넓히며, 관리자가 그것을
      끈 데에는 이유가 있다. 그 판단을 뒤집어 달라는 요청임을 인정하고 물어야 한다.
- [ ] **(B) `bwrap`에 setuid 비트 요청** — `chmod u+s`. userns 없이도 동작한다.
      (A)보다 국소적이지만 setuid-root 바이너리를 하나 늘리는 일이다.
- [ ] **(C) 앱 전용 OS 계정 + `sudo -u` 허용 요청** — 파일 권한만으로 대부분이
      해결된다. 다만 요청이 둘(계정 + sudoers)이고, 네트워크 차단은 별도로 남는다.
- [x] **(D) Landlock + seccomp 폴백 자체 구현** — **선택함.** 커널 6.8 확인(2026-09-06)이라 Landlock ABI 4(네트워크 제한 포함)를 쓸 수 있다.
      Ubuntu 24.04에서 userns 차단은 관리자의 추가 강화가 아니라 **배포판 기본값**이므로,
      그것을 꺼 달라고 요청하는 것보다 자체 격리를 갖는 편이 방어 가능하다고 판단.
  - [x] `gitea deptapp-exec` 서브커맨드 (`company/deptappexec.go`, `cmd/main.go` 1줄)
  - [x] Landlock (`company/sandbox_apply_linux.go`) — ABI 조회 후 마스크 절삭(모르는 권한을 handled에 넣으면 호출 전체가 실패한다), 읽기 전용/쓰기 경로 규칙, TCP bind·connect 차단
  - [x] seccomp (`company/sandbox_seccomp_linux.go`) — 손으로 쓴 BPF, 새 의존성 0
    - [x] `socket(AF_INET/AF_INET6/AF_PACKET/AF_NETLINK)` 거부 (Landlock은 TCP만 막으므로 **UDP·raw는 여기서만 막힌다**)
    - [x] `ptrace`·`process_vm_readv/writev` 거부 — PID 네임스페이스가 없어 Gitea 메모리가 같은 UID에 노출된다
    - [x] `kill(-1)`·`kill(giteaPID)` 거부 + `pidfd_send_signal`·`rt_sigqueueinfo` 등 우회 경로 차단
    - [x] **아키텍처 검증** — 없으면 64비트 프로세스가 32비트 syscall 번호로 전 규칙을 우회한다
  - [x] rlimit을 헬퍼가 직접 `setrlimit` — `prlimit(1)` 의존성 제거 (자기 자신이 자식이므로 이게 정공법)
  - [x] `/proc` 전체 차단(PID 네임스페이스 대체) + `/proc/self`만 허용
  - [x] `/tmp` 대신 앱별 전용 `HOME`·`TMPDIR` — 마운트 네임스페이스가 없어 호스트 `/tmp`가 전 앱 공유다
  - [x] Python 기동에 필요한 `/etc/passwd`·`group`·`nsswitch.conf`·`localtime` 읽기 허용 (`/etc/shadow`는 제외)
  - [x] 3단 선택 로직 — bubblewrap > Landlock > (옵트인 시) 무격리. bwrap이 더 강하므로 가능하면 그쪽
  - [x] 테스트: BPF 프로그램을 **커널과 같은 방식으로 해석해** 점프 오프셋 검증 (오프셋 하나 틀리면 조용히 허용된다), ABI 마스크 절삭
    - [ ] ⚠ seccomp 단위 테스트 **미실행** — 리눅스 전용 빌드라 macOS에서 돌릴 수 없다
  - [x] **실기에서 버그 1건 발견 (2026-09-06)**: `landlock_add_rule for /etc/resolv.conf:
    invalid argument`. Landlock은 **디렉터리 전용 권한을 파일에 주면 `EINVAL`**을 낸다
    (`READ_DIR`·`MAKE_*`·`REMOVE_*`·`REFER`). 허용 목록에 `/etc/resolv.conf`·`/dev/null`·
    `/dev/urandom`이 있어 코너 케이스가 아니라 **항상 실패**했다. `fstat`으로 디렉터리
    여부를 보고 파일이면 마스크를 줄이도록 수정. 리눅스에서 실제로 돌리지 않으면
    나올 수 없는 종류의 버그다
  - [ ] 실기 검증 (아래 "샌드박스 실증" 항목 전체) — 리눅스 서버 필요

- [x] 진단 (2026-09-06, 배포 서버에서 `preflight.sh`):
  - 커널 `6.8.0-136-generic` (Ubuntu 24.04 계열)
  - **Landlock: filesystem + network (ABI 4+)** — TCP bind/connect 제한을 커널이 해 준다
  - **`landlock lsm enabled: yes`** — LSM 목록에 실제로 올라와 있다. 커널에 컴파일만 되고
    활성화되지 않은 경우가 있어 이 줄이 없으면 위 판정은 의미가 없다
  - ⚠ 다만 `preflight.sh`는 커널 버전으로 **추론**할 뿐 `landlock_create_ruleset`을 실제로
    호출하지 않는다. 확정은 `run.sh`(또는 Gitea 기동 시 `landlockProbe`)에서 난다
  - **bubblewrap: 사용 불가.** `--unshare-user`로 직접 시험하면
    `bwrap: setting up uid map: Permission denied` — `unshare -Ur`와 같은 원인이다.
    - 처음에 `--unshare-all`로 시험했을 때 `loopback: FAILED RTM_NEWADDR`가 나와
      "네임스페이스는 만들어졌다"고 오판했다. **`--unshare-all`은 `--unshare-user-try`로
      확장되고, `-try`는 실패해도 조용히 넘어간다** — 그래서 userns 없이 진행하다가
      관계없는 지점에서 터진 것이다
    - → `sandboxProbe`와 `preflight.sh`를 `--unshare-user`(하드 실패)로 수정했다.
      고치지 않았다면 **격리 없이 성공하는 호스트를 "bubblewrap 가능"으로 오판**할 수 있었다
  - **결론: Landlock + seccomp 단독.** 이미 구현·테스트 완료

## 1. 기반 정리 (기능 변화 없음, 독립 가치)

- [x] 로케일 `company.*` 키가 `locale_en-US.json` 맨 끝 한 블록인지 확인 — **이미 연속이었다**(121키 span 121). 앞선 "뒤섞여 있다"는 분석은 검사 범위를 블록 앞에서부터 잡은 실수였다. 조치 불필요, 앞으로도 새 키는 파일 끝에 추가할 것
- [x] `docs/company/patches.md`의 stale 행 3개 실제 상태로 수정
- [x] **스냅샷 delete 버그 수정** (`company/deploy.go:889` — upload만 내보내고 삭제 미반영)
  - [x] `deployFilePreviews`의 before/after 집합 계산을 공용 헬퍼로 추출 (미리보기와 커밋이 같은 근거를 쓰게)
  - [x] `beforePaths \ afterPaths`에만 delete 연산 생성 (전체 재귀 삭제 금지 — 최초 배포가 `handleCheckErrors`에서 통째로 실패한다, `update.go:363`)
  - [x] `len(files)==0` 가드를 "upload도 delete도 없을 때"로 (마지막 파일을 지운 부서가 되돌릴 수 있게)
  - [x] 테스트: 삭제만 / 최초 배포 / 리네임 / 파일↔디렉터리 충돌 (`company/deploy_test.go` 확장)
- [x] `[actions] ENABLED = false` — **지금도 직원이 자기 repo에 워크플로를 심을 수 있다.** 재활성화는 별도 변경 절차 필요(app-platform.md에 근거 기록됨)
  - [x] 확인됨 (2026-09-06): 저장소에서 Actions 탭이 사라졌다. `modules/setting/repository.go`가 이 값을 보고 `repo.actions`를 `DisabledRepoUnits`에 넣으므로, 워크플로 파일이 존재해도 인스턴스 전역에서 아무 일도 일어나지 않는다
- [x] Gitea 로그 `MODE = file`로 (현재 console — 재시작하면 감사 기록이 사라진다)
- [x] 기존 AI API 키 평문 저장 수정 — `company/secretstore.go` 신설(`setUserSecret`/`getUserSecret`), 평문은 읽기 시 1회 마이그레이션. **환경변수 저장이 이 헬퍼를 그대로 재사용한다**
  - [x] 확인됨 (2026-09-06): `user_setting` 테이블의 `company.ai.api_key`가 hex 320자로 저장되고, AI 기능도 정상 동작한다
  - [ ] 평문 마이그레이션 대기 1건 — 아직 AI를 한 번도 쓰지 않은 계정은 평문 그대로다.
    일괄 변환을 돌리지 않고 **읽는 시점에 1회** 마이그레이션하도록 만들었기 때문이며(`getUserSecret`),
    해당 계정이 AI를 쓰거나 키를 다시 저장하면 자동으로 정리된다. 확인:
    `sqlite3 data/gitea.db "select user_id from user_setting where setting_key='company.ai.api_key' and setting_value like 'sk-%'"`

## 2. 상태 배관 (호스트 불필요, curl로 검증)

- [x] `company/appstate.go` — 상태 JSON 읽기/쓰기 (per-key 뮤텍스 + temp+rename, `workspace_tmp.go` 패턴)
  - [x] `desired_state`/`actual_state` 분리, 원인 코드 필드, 이력(최근 10)
  - [x] 손상 파일은 기본값 재생성 + 관리자 경고 (fail-open)
- [x] merge 시점 seed — `deploy_notifier.go`의 `cleanupDeployBranch`에 `queued` 기록 추가 (부서 해석 로직 이미 있음, `:77`)
- [x] 비개발자 배지 확장 — `deploystatus.go:67`의 `pr.HasMerged` 분기에서 상태 파일 조회, `배포 중`/`배포됨`/`배포 실패` 매핑 (**Go에서 매핑** — `rolled_back`을 직원에게 노출하지 않는다)
  - [x] `DeployStatus` JSON에 `message`·`pid`·헬스 상세 **비노출** (게이트 default-allow 경로다)
  - [x] 배지 라벨·색상·로케일 (en/ko)
- [x] 관리자 목록 화면 골격 — `/-/admin/company-deploys` 읽기 전용 (`admin_activity.tmpl` 관례). 배포한 적 없는 부서도 행으로 표시, 저장소가 사라진 고아 상태는 '조치 필요'로
  - [ ] ⚠ **미검증**: 관리자로 로그인해 화면이 실제로 렌더링되는지 (기동 시 템플릿 파싱 오류는 없음)
- [x] 테스트: 상태 전이표, 손상 파일 복구, 키 충돌, 이력 상한, 롤백 대상, `-race` 동시 갱신

## 3. 프로세스 관리 + 프록시 (macOS에서 개발 가능)

- [x] `company/appproc.go` — supervisor (앱별 뮤텍스, 상태 머신, impl.md §2)
  - [x] 기동/종료 (`SIGTERM`→10초→`SIGKILL`, `Setpgid`)
  - [x] `cmd.Env` **명시 구성** — 부모 환경 상속 금지 (impl.md §5, 이게 빠지면 Gitea 시크릿이 통째로 샌다)
  - [x] 크래시 백오프 재시작, 3연속 실패 시 `failed`
  - [x] Gitea 기동 시 재조정 1회 (desired 존중) — `company/appinit.go`, `routers/init.go` 1줄
    - [ ] stale 소켓 정리를 PID 확인 후로 좁히기 (현재는 자식이 없을 때만 도달하므로 무조건 삭제)
- [x] `company/envstore.go` — 앱별 환경변수 (암호화 저장, 이름 검증, 버전 카운터, 변경 이력)
  - [ ] `_app` 화면에서 입력받기 (6단계)
- [x] `company/appsandbox.go` — `buildAppCommand` (bwrap / dev 분기, `${SOCKET}`·`${ROOT_PATH}` 치환)
- [x] 배포 워커 `company/deployworker.go` — 큐 + 워커 2개, 같은 앱 직렬화(`deployMu`), 큐 상한 초과 시 `deploy_queue_full` (merge 훅은 큐잉 후 즉시 반환)
  - [x] central-deploy에서 `<owner>/<repo>/` 서브트리를 릴리스로 추출 (경로 탈출 차단, 실행 비트 없음)
  - [x] venv 생성 + `pip install --only-binary=:all:`
  - [x] venv 캐시 — **requirements 집합** 기준 (파일 바이트가 아니라 정규화·정렬된 `name==version`, 주석·순서 변경으로 재빌드되지 않게)
  - [x] **설치 후 `pip list` 대조** — 전이 의존성까지 허용 목록 검사. 승인 안 된 것이 들어오면 배포 실패 (wheel만 설치하므로 이 시점까지 코드는 실행되지 않는다)
  - [x] `current` 심볼릭 링크 원자 교체 (임시 링크 + `rename`)
  - [x] 릴리스 GC (`company/releasegc.go`) — 배포 성공 후 `deployMu` 안에서만 실행. **최근 5개** 유지(상태 파일의 이력 상한보다 크면 롤백할 수 없는 릴리스가 남는다)
    - [x] `current`·`previous`가 가리키는 릴리스는 **나이와 무관하게, 슬롯도 차지하지 않고** 보존 — 몇 달 배포가 없던 앱이 자기가 서비스 중인 릴리스를 잃으면 안 된다
    - [x] venv는 나이가 아니라 **참조 기준**으로 수거 — 같은 의존성 집합이면 여러 릴리스가 공유하므로, 가장 새 venv가 오래된 릴리스의 것일 수 있다
    - [x] 테스트: 최신 N 유지, current/previous 보호, 공유 venv 생존, 고아 venv 삭제, 미배포 앱 no-op
- [x] 헬스체크 (impl.md §7 — 연속 3회) + 자동 롤백 (`rolled_back` vs `failed` 구분)
  - [ ] 크래시 루프 재확인(10초 후 재시작 횟수 비교) — 현재는 연속 성공 3회만
- [x] `company/proxy.go` — `/apps/{owner}/{repo}/*` 리버스 프록시 (`vitedev.go:61` 패턴 + unix 소켓 DialContext), `optSignIn` 없이 등록해 무인증 유지
  - [x] 인메모리 앱 레지스트리 (`company/appregistry.go`) — 요청당 파일 읽기 없음, 미등록 404. URL을 소켓 경로로 직접 바꾸지 않는 **보안 경계**이기도 하다
  - [x] 접근 모드 3종 (`public`/`login`/`org`), 들어오는 `X-Forwarded-*`·`X-Gitea-*` 스트립 후 우리가 다시 설정. `org` 판정 실패는 **fail-closed**
  - [x] 다운로드 정책 (`Content-Disposition` 차단, Content-Type 화이트리스트, 크기 상한 + 청크 응답용 스트림 캡, 차단 기록)
  - [x] 프록시 캐시가 정책을 **클로저에 가두지 않도록** 매 요청 `SettingsFor` 재조회 (안 그러면 `apps.yml` 변경이 그 앱에 영원히 반영 안 됨)
  - [x] `/apps/`를 게이트 `uiWhitelist`에 명시
- [x] `.company/apps.yml` 파서 (`company/appconfig.go`) — defaults+오버라이드, **파싱 실패 시 마지막 정상 설정 유지** (돌던 앱을 죽이지 않는다)
- [x] requirements 검증 (`company/requirements.go` — impl.md §5 정규식, 거부 목록) + 허용 목록 대조
- [x] 테스트: 파서(깨진 YAML 포함), requirements 검증, 환경변수 이름·암호화·all-or-nothing, 경로 탈출, venv 캐시 키, 심볼릭 링크 교체, 큐 초과, `-race`
  - [x] 프록시 정책 전종 — 레지스트리(대소문자·미등록·경로 탈출), 헤더 스트립 목록, Content-Type 화이트리스트, `Content-Disposition`·크기 상한·스트림 캡, `policy: allow` 무력화, 상태 코드 기록

## 4. 샌드박스 (Linux 필요)

- [x] `gitea deptapp-exec` 없이 직접 bwrap 기동 (impl.md §6 커맨드) — `cmd/main.go` 수정 불필요해짐, bwrap을 Gitea가 직접 exec
- [x] rlimit — `prlimit(1)`로 bwrap을 감싸 적용. Go의 `os/exec`에는 fork-exec 사이 훅이 없어 자식에만 `setrlimit`을 걸 수 없고, 부모에 걸면 **Gitea 자신이 앱 한도로 잘린다**
  - [x] `RLIMIT_DATA` 선택 (`RLIMIT_AS`는 numpy류의 가상 예약 때문에 정상 앱을 죽인다) + `--nproc`·`--nofile`
  - [x] `prlimit` 부재 시 **fail-open** (격리가 아니라 공평성 문제이고 메모리는 watchdog이 덮는다) — 관리자 화면에 표시
  - [ ] 실측 비교 (`RLIMIT_DATA`가 실제 파이썬 앱에서 충분한지) — Linux 서버 필요
- [x] `nice` 10 + `oom_score_adj` 500 (`company/applimits_linux.go`) — **커널 OOM이 Gitea를 고르면 플랫폼 전체가 내려간다**
- [x] tmpfs 크기 상한 (`--size` + `--tmpfs /tmp`)
- [x] **fail-closed**: bwrap 부재·userns 차단 시 기동 거부 + `[company] ALLOW_UNSANDBOXED_APPS` 옵트인으로만 예외 (비Linux는 개발용으로 자동 허용)
- [x] **검증 도구 작성** — `docs/company/sandboxcheck/` (앱을 배포하지 않고 `deptapp-exec`를 직접 호출한다)
  - [x] `preflight.sh` — 커널·bubblewrap 가용성과 실패 사유·Landlock 수준·PyPI 접근성
  - [x] `probe.py` — Gitea 데이터 / 네트워크 / 타 프로세스 / 자원 한도 + **"막히면 안 되는 것"**(이게 없으면 "전부 막힘"과 "파이썬이 아예 못 뜸"을 구분할 수 없다)
  - [x] `run.sh` — **통제군 먼저**: 샌드박스 밖에서 돌려 프로브가 실제로 구분한다는 것을 증명한 뒤 본 실행. 통제군 실패 시 중단
  - [x] 통제군이 실제로 결함을 **세 번** 잡아냄. 전부 "샌드박스와 무관하게 이미 막히는 것"이라
    본 실행에서 통과로 보이며 아무것도 증명하지 않았을 항목들이다:
    - `/etc/shadow`·`/usr` 쓰기 — root 전용. 계약에서 제외, defence-in-depth로 강등
    - `AF_PACKET` raw 소켓 — `CAP_NET_RAW` 필요. 비특권 계정은 애초에 못 만든다
    - `ptrace` — Ubuntu 기본 **Yama `ptrace_scope=1`**이 형제 프로세스 추적을 막는다.
      호스트마다 다르므로 `/proc/sys/kernel/yama/ptrace_scope`를 **실행 시 읽어** 판단하게 했다
- [x] **통제군 실행 확인 (2026-09-06, 배포 서버)** — 격리가 없으면 앱이 `gitea.db`·
  `data/sessions/`·전 부서 저장소·`app.ini`를 전부 읽고, Gitea의 환경변수를 읽고,
  `kill(-1)`까지 할 수 있다는 것이 이 서버에서 실증됐다. 설계 문서의 주장이 추정이 아님
- [ ] **샌드박스 실증 실행 (서버에서, 전부 `ok`여야 통과):**
  - [x] `./preflight.sh`
  - [x] `./run.sh` **1회차 (2026-09-06)** — 실제로 막히는 것이 확인된 항목:
    `gitea.db` / `data/sessions/` / 전 부서 저장소 / `app.ini` / `/etc/shadow` /
    `/usr` 쓰기 / **릴리스 트리 쓰기**(자기 코드 덮어쓰기 = 영속 백도어) /
    TCP·IPv6·raw·netlink·UDP·DNS·`curl` / `/proc` 나열 / Gitea `environ` /
    ptrace / Gitea에 시그널 / **`RLIMIT_DATA` 초과 할당(`MemoryError`)**
  - [x] "막히면 안 되는 것" 6개 전부 통과 — **파이썬이 정상 동작한다.**
    자기 디렉터리 쓰기 / `/proc/self` / unix 소켓 / `ssl` / `/etc/passwd` / `getpass.getuser()`
  - [x] Gitea 환경변수 유출 0건
  - [x] **실패 1건 → 수정함**: `kill(-1)`이 뚫렸다. `pid_t`는 32비트이고 x86-64는 레지스터
    상위 절반을 미정의로 두므로, glibc가 `-1`을 `edi`에 넣으면 **제로 확장**되어 커널이
    보는 `args[0]`은 `0x00000000FFFFFFFF`다. 상위 워드만 검사하던 필터가 그대로 통과시켰다.
    **단위 테스트가 `^uint64(0)`(부호 확장)만 검사해서 실제 경우를 놓쳤다** — 두 형태 모두
    검사하도록 추가. 이제 음수 pid 전체를 거부한다(`killpg` 불가는 감수)
  - [x] `./run.sh` **2회차 (2026-09-06) — `All checks passed: the sandbox holds.`**
    통제군에서 전부 뚫리고 샌드박스에서 전부 막혔다. 같은 프로브, 같은 호스트, 같은 계정이므로
    **격리가 실제로 동작한다는 증명**이다. 이 프로젝트 최대의 미검증 위험이 해소됐다
  - [x] `./gitea deptapp-exec --self-test` **(2026-09-06) — `All checks passed.`**
    `landlock_create_ruleset`을 실제로 호출해 **ABI 4 확정**(커널 버전 추론이 아님).
    외부 통신 허가 분기에서도 ptrace·kill이 막히는 것, 32비트 우회 차단, BPF 오프셋 전부 확인
    - [x] 출력에서 `MAKE_CHAR`·`MAKE_BLOCK`이 handled 마스크에 빠진 것을 발견해 추가
      (`0x77bf` → `0x7fff`). Landlock은 **handled에 없는 권한을 제한하지 않는다.**
      실제 노출은 없었다 — `mknod`에는 `CAP_MKNOD`가 필요하고 비특권 계정엔 없다.
      구멍이 아니라 마스크가 덜 완전했던 것
  - [ ] 실제 토이 앱 배포 후 `subprocess` 다중 자식 → 프로세스 그룹 종료로 고아 0인지 (Landlock에는 PID 네임스페이스가 없어 여기가 bwrap보다 약한 유일한 지점)

## 5. 모니터링

- [x] 지표 수집 (`company/metrics.go`) — 앱별 작은 구조체 뮤텍스, 요청당 파일·DB 접근 0, 백그라운드 flush 1분
  - 계획의 "atomic 카운터"에서 변경: p95용 표본과 고유 방문자 집합이 필요해 atomic만으로는 안 된다. 경합 범위가 **앱 하나**로 좁아 실질 비용은 같다
- [x] 자원 샘플링 (`company/appsample.go`) — `/proc` 5초 주기, **프로세스 트리 합산**(자식이 진짜 사용처다), watchdog과 동일 goroutine
- [x] 버킷 스키마 (요청·사용자·상태코드·응답시간 p95 + mem/cpu/threads/fds avg·max + disk + restarts + blocked)
  - [x] **평균과 함께 최대를 저장** — 한도 상향 판단의 근거는 평균이 아니라 피크다
- [x] watchdog — VmRSS 연속 3회 초과 시 `Stop`(TERM→10초→KILL), `oom` 기록 (스파이크 1회로 죽이지 않는다). `desired`는 `running` 유지 — 부서가 끈 게 아니다
- [x] 로그 수집 — stdout/stderr 파이프 → 회전 파일 (10MB×5)
- [x] 로그 조회 (`company/applogs.go`) — 검색, 스트리밍 읽기 + 5초 타임아웃 + 결과 상한(링 버퍼), **부분 문자열 기본·정규식은 옵트인** (ReDoS)
- [x] 앱 상세 — KPI 타일 + 차트 3종 + **배포/롤백/OOM 시점 세로선 annotation** + 이력 타임라인 (`custom/templates/company/admin_app.tmpl`)
  - chart.js를 직접 사용(`web_src/js/features/company-app-charts.ts`). `ChartCanvas.vue`는 Vue 컴포넌트라 정적 데이터 3개에 Vue를 띄울 이유가 없다. **새 의존성 없음**
- [ ] 전체 대시보드에 KPI 타일 + 앱별 스파크라인 추가 (현재 목록은 상태·헬스만)
- [ ] 로그·앱 이름 등 직원 입력 **이스케이프 확인** (`<script>` 주입 테스트) — 템플릿은 기본 이스케이프이고 `innerHTML` 미사용, 실제 주입 확인은 미실시
- [x] 테스트: 버킷 집계(평균·최대·p95·고유 방문자), 자원 샘플 최대값 보존, 파일 왕복 + 기간 필터, `-race`(동시 기록 50)

## 6. 제어 + 부서 화면

- [x] 제어 액션 (`company/app_control.go`) — start/stop/restart/rollback(부서·관리자), suspend/resume/remove(관리자만)
  - [x] `stopped` vs `suspended` 구분 — suspended는 부서가 못 켬(버튼 자체가 없다), 사유 표시
  - [x] 이력 (누가·언제·왜)
  - [x] 롤백은 `previous`↔`current` 교환 — 두 번 롤백하면 제자리로 돌아온다(앱이 붕 뜨지 않는다)
  - [x] **실행 중인 버전을 릴리스 자신이 기록한다** (`company/appreleaseid.go`) — 릴리스 디렉터리는
    `sha256(sha)[:16]`이라 심볼릭 링크에서 커밋을 역산할 수 없었고, 상태 파일의 SHA는 *실행된
    것*이 아니라 *의도된 것*이 쓰던 필드였다. 롤백이 링크만 옮기고 그 필드를 두고 가서 화면은
    실패한 커밋을 가리키고 "다시 배포"가 그 커밋을 다시 빌드했다. 이제 릴리스 안의 `sha` 파일이
    기록이고, 롤백·기동 재조정이 디스크에서 되읽는다(캐시 아님)
  - [x] 자동 롤백 후 `previous`가 `current`와 같아지던 문제 — 다음 롤백이 같은 버전을 재시작하며
    아무 일도 안 한 것처럼 보였다
- [x] `/{owner}/{repo}/_app` 부서 화면 (**`_deploy` 아님** — `/deploy`가 이미 존재)
  - [x] 접근 권한 3종 표시 (결과를 평문으로 설명), 넓히는 방향은 요청 링크로 연결
  - [x] 좁히는 방향을 화면에서 즉시 적용 (`company/appaccess.go`) — 부서 선택은 상태 파일에 남고
    `SettingsFor`가 정책과 **더 좁은 쪽**을 취한다. 따라서 여기 저장된 값이 정책에 없는 접근을
    열어줄 수 없고, 관리자가 `apps.yml`을 조이면 즉시 이긴다. 선택 가능 범위는 부서의 현재
    선택이 아니라 **정책 천장**(`configuredAccess`) 기준 — 아니면 한 번 좁히면 되돌릴 수 없다.
    프록시 핫패스라 인메모리 맵을 함께 두되 기동 시 상태 파일에서 복원한다(유일한 사본이 아니다)
  - [x] 환경변수 — 암호화 저장(`EncryptSecret`), `(변경하지 않음)` 패턴, 이름 검증(`LD_*` 등 거부), 변경 이력(값 제외), **"재시작 필요" 표시**(저장 버전 vs 실행 버전)
  - [x] 실패 원인 요약 (`company/appcause.go`) — 원인 코드 → 평문 + **다음 조치 버튼**. 원문 로그는 관리자 전용
- [x] 저장소 사이드바 패널 — 상태, 권한 현황(**거부·대기 포함** + 사유 + 요청 링크), 실패 원인 요약, 메모리 사용량/한도
  - [x] `home_sidebar_top.tmpl` 비관리자 분기에만 추가 (관리자 분기 불변 — 드리프트 최소화)
  - [x] 배포된 적 없는 저장소에는 아예 렌더링 안 함 (평범한 저장소 사이드바 불변)
  - [x] 인메모리 값만 사용 — 인스턴스에서 가장 많이 열리는 페이지에 파일 읽기·쿼리 추가 금지
  - [x] **저장소 헤더에서 직접 시작/중지** — "Deploy Request" 옆. 비개발자가 실제로 여는
    화면이 여기이고, "우리 앱 한 시간만 내려주세요"는 관리자에게 부탁할 일이 아니라
    부서가 결정할 일이다
    - [x] `suspended`면 시작 버튼을 **렌더링하지 않는다** — 관리자의 보호 조치를 부서가
      즉시 되돌릴 수 있으면 그 조치가 무의미해진다
    - [x] 중지는 확인을 받는다 ("지금 사용 중인 사람들이 접속할 수 없게 됩니다")
    - [x] 누른 자리로 돌아온다 (`RedirectToCurrentSite`가 외부 referer를 거부하므로
      open redirect가 되지 않는다)
    - [x] 배포된 적 없는 저장소에는 아무것도 렌더링되지 않는다
    - 주의: `SetAppPermissionData`가 저장소 홈 라우트에만 걸려 있어 버튼도 거기에만 보인다
- [ ] 검증: 부서가 끈 앱이 Gitea 재시작 후에도 꺼져 있는지 / suspended를 부서가 못 켜는지 / 환경변수 값이 저장 후 어디에도 재노출 안 되는지 / `ps aux`에 값 없는지 / 재시작 전 옛 값·후 새 값

## 7. 권한 요청·승인 흐름

- [x] 자동 감지 로직 (`company/permissions.go`) — requirements → 미승인 패키지, 한도 도달 횟수 → 상향 제안(**근거 문구 포함**), 접근 권한은 **넓힐 때만** 요청
- [x] 요청 저장·조회 (`SavePermissionRequests`/`LoadPermissionRequests`, PR id로 키잉해 이전 요청이 새 PR에 붙지 않게)
- [x] 승인분을 `AppSettings`에 반영하는 순수 함수 (`ApplyApprovedPermissions`) — 패키지는 `allowExtra`(부서별), 접근/메모리/다운로드/네트워크
- [x] Deploy Request 화면에 감지 결과 주입 (`DeployForm` → `PermissionRequests`)
- [x] Deploy Request 템플릿에 체크박스 + **사유 필수** 입력란 (`deploy.tmpl`)
- [x] 요청 상세 화면에 "요청된 권한" 섹션 (`deploy_request_files.tmpl`) — 코드 diff 바로 아래. 별도 화면으로 나누면 승인이 도장 찍기가 된다
  - [ ] 항목별 개별 승인/거부 — 현재는 **merge = 전체 승인**. 일부만 승인하려면 요청자에게 수정 요청 (화면에 명시)
- [x] merge 시 승인분 `apps.yml` 커밋 (`company/permissions_commit.go`) — 작성자=관리자이므로 **커밋 자체가 감사 기록**, 낙관적 재시도 3회, 커밋 후 인메모리 정책 즉시 갱신
  - [x] 파싱 불가한 기존 `apps.yml`은 **덮어쓰지 않고 거부** (관리자가 손으로 쓴 내용을 날리지 않는다)
  - [x] 기동 시 `LoadAppsConfigFromRepo` — 재시작해도 승인된 정책이 유지된다 (없으면 전 앱이 조용히 기본값으로 되돌아간다)
- [ ] 정책 탭 `/-/admin/company-deploys/policy` — 허용 패키지 전체 목록, 대기 요청 모음, git log 링크
- [x] 관리자 navbar 오버라이드 — 두 링크(배포 관리 + 기존 company-activity). `custom/templates/admin/` 첫 오버라이드이고, **기존 `company-activity`는 지금까지 링크가 아예 없어 URL 직접 입력으로만 접근 가능**했다. `patches.md` 기록 완료
- [x] 테스트: 감지(미승인 패키지만·넓힐 때만·근거 있을 때만), 반영(`allowExtra`·거부는 무시·PEP 503 중복 방지), PR id 키잉, 권한 파일이 앱 목록에 안 섞이는지
- [ ] 검증: 거부 항목이 부서 화면에 사유와 함께 보이고 요청으로 이어지는지 (실제 브라우저)

## 8. 송신 브로커 (외부 통신 필요 부서가 생길 때)

- [ ] 브로커 엔드포인트 — 호스트·메서드·경로 허용 목록, 관리자 자격증명 주입, 전수 감사 로그
- [ ] `network.mode: broker` 배선 + 부서용 호출 규약 문서화 (3줄 정형 코드)
- [ ] `open` 모드 관리자 경고 표시

## 9. 마무리

- [ ] 운영자 리허설 — **비개발자가 셸·YAML·git 없이**: 승인, 거부(사유), 한도 상향, 재시작, 실패 원인 파악. **하나라도 파일을 열어야 하면 설계 결함**
- [ ] 업스트림 머지 리허설 — 드리프트 검사 스크립트 실행 (`custom/templates` 전 파일 대조)
- [ ] `architecture.md`에 이 플랫폼 반영 (push mirror 서술 교체)
- [ ] 전 구간 E2E — 편집→요청→승인→배포→`/apps/` 응답→배지 "배포됨"

---

**매 단계 공통:** `make fmt` (Go 수정 후) · `make lint-go` / `lint-templates` / `lint-js` · 동시성 테스트 `-race` · 커밋 메시지 Conventional Commits + `Assisted-by` 트레일러
