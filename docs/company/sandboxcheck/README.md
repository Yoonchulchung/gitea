# 샌드박스 검증

배포된 부서 앱이 **Gitea와 같은 OS 계정으로 실행된다.** 배포 서버에 root가
없어 부서별 계정을 만들 수 없기 때문이다. 그래서 격리가 유일한 방어선이고,
그것이 실제로 동작하는지는 주장이 아니라 확인의 대상이다. 격리가 없으면 앱이
`data/sessions/`를 읽어 **관리자로 즉시 로그인**할 수 있다 — 비밀번호도
필요 없다.

배경은 [app-platform.md](../app-platform.md)에 있다.

## 언제 돌리나

- 배포 서버에 처음 올릴 때 (**앱을 하나라도 배포하기 전에**)
- 커널 업그레이드 후 — Landlock과 user namespace 정책이 모두 커널에 달려 있다
- 업스트림 Gitea 머지 후 — 샌드박스 코드가 그대로인지 확인
- 관리자 화면에 "격리 없이 실행 중" 경고가 뜰 때

## 1. `preflight.sh` — 이 호스트가 무엇을 할 수 있나

```sh
./preflight.sh
```

인자도 권한도 필요 없다. 커널·배포판, bubblewrap 가용 여부와 실패 사유,
Landlock 지원 수준, python3와 PyPI 접근성을 한 화면에 보여준다.
**앱을 배포하기 전에** 돌린다.

## 2. `run.sh` — 격리가 실제로 막는지

```sh
./run.sh /path/to/gitea /path/to/gitea/data
```

`gitea` 바이너리와 데이터 디렉터리 경로가 필요하다. **Gitea를 실행하는 그
계정으로** 돌려야 한다 — 질문 자체가 "그 계정이 샌드박스 안에서 무엇에 아직
닿는가"이기 때문이다. 앱을 배포할 필요는 없다. `gitea deptapp-exec`를 직접
호출한다.

두 번 실행된다:

1. **통제군** — 샌드박스 **밖**에서. "막혀야 하는" 항목이 전부 뚫려야 한다.
   여기서 실패하면 프로브가 샌드박스 안팎을 구분하지 못한다는 뜻이므로,
   본 실행도 아무것도 증명하지 못한다. 그래서 통제군이 실패하면 거기서 멈춘다.
2. **본 실행** — 실제 앱과 동일한 제한으로.

프로브가 시도하는 것:

| 분류 | 항목 |
|---|---|
| Gitea 데이터 | `gitea.db`, `data/sessions/`, 전 부서 저장소, `app.ini`, `/etc/shadow`, `/usr` 쓰기, 자기 릴리스 트리 쓰기 |
| 네트워크 | TCP·IPv6·raw·netlink 소켓, UDP, DNS, `subprocess curl` |
| 타 프로세스 | `/proc` 나열, Gitea `environ` 읽기, ptrace, 시그널, `kill(-1)` |
| 자원 한도 | 메모리 한도 초과 할당 |

**"막히면 안 되는 것"도 함께 본다** — 자기 디렉터리 쓰기, `/proc/self`,
unix 소켓, `ssl` import, `/etc/passwd`, `getpass.getuser()`. 이게 없으면
"전부 막혔다"와 "파이썬이 아예 뜨지 못했다"를 구분할 수 없고, 후자가 성공처럼
보인다.

마지막으로 `os.environ`에 Gitea 변수가 섞였는지 확인한다. 샌드박스가 파일을
막아도 환경변수로 새면 의미가 없다.

## 3. seccomp 필터 단위 테스트

BPF 점프 오프셋은 **하나만 틀려도 조용히 허용된다.** 그래서 커널과 같은
방식으로 프로그램을 해석하는 테스트가 있고, 리눅스에서만 빌드된다:

```sh
go test -run 'Seccomp|HandledRights' ./company/
```

서버에 Go가 없으면 개발 머신에서 만들어 옮긴다:

```sh
GOOS=linux GOARCH=amd64 go test -c -o company-linux.test ./company/
./company-linux.test -test.run 'Seccomp|HandledRights' -test.v
```

## 결과 읽기

`FAIL`이 **하나라도** 있으면 격리가 설계대로 동작하지 않는 것이다. 그 호스트에
부서 앱을 올려서는 안 된다. 플랫폼은 이 상황에서 앱 기동을 스스로 거부하지만
(fail-closed), `[company] ALLOW_UNSANDBOXED_APPS`로 그 거부를 끌 수 있으므로
이 검증이 그 설정을 켜도 되는지에 대한 유일한 근거다.

`SKIPPED`는 통과가 아니다 — 호스트에 그 대상이 아예 없어 시험하지 못했다는
뜻이며, "막혔다"와 다르다.
