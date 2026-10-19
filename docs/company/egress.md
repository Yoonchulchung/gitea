# 이그레스 — 이 플랫폼에서 밖으로 나갈 수 있는 모든 경로

**왜 이 문서인가.** 사내 네트워크는 특정 사이트와 외부 AI 사용을 금지한다. 허가되지
않은 패킷 하나가 사고다. 그래서 "밖으로 나갈 수 있는 경로"를 코드와 설정 양쪽에서
**전부 열거**하고, 각각을 닫는 스위치를 옆에 둔다. 새 외부 호출을 추가하는 사람은 이
표에 행을 추가해야 한다 — 표에 없는 경로는 존재하면 안 된다.

프로덕션 호스트에서 실측: `docs/company/scripts/check-egress.sh`. 개발 노트북의
결과는 프로덕션에 대해 아무것도 말해 주지 않는다.

## 경로와 스위치

| 경로 | 어디서 | 목적지 | 닫는 스위치 | 기본 |
|---|---|---|---|---|
| AI 요청 (Anthropic 직결) | `company/ai.go` | `api.anthropic.com` | `[company] AI_ENABLED` | **꺼짐** |
| AI 요청 (OpenAI 호환 게이트웨이) | `company/ai.go` | `AI_API_URL` | `AI_ENABLED`, `AI_API_URL` 비움 | 꺼짐 |
| 모델 목록 조회 | `company/settings_ai.go` | `AI_API_URL` | `AI_ENABLED` | 꺼짐 |
| 패키지 설치 (배포마다) | `company/deployworker.go`, `depresolve.go` | `PIP_INDEX_URL` 또는 pypi.org | `PIP_INDEX_URL` 설정, `PIP_ALLOW_PUBLIC_INDEX` | **인덱스 없으면 배포 거부** |
| 앱의 아웃바운드 (브로커) | `company/broker.go` | apps.yml 허용목록 | `network.mode` (`none`/`broker`/`open`) | `none` |
| 아바타 | Gitea | gravatar.com | 관리자 패널 → Configuration (`system_setting`의 `picture.disable_gravatar`) — **app.ini 키는 이 버전에서 무시됨**(deprecation 오류) | 내장 기본 꺼짐; DB에 정책으로 명시함 |
| 웹훅 | Gitea | 사용자가 입력한 URL | `[security] ALLOWED_HOST_LIST` | 빈 목록 (설정함) |
| 미러 | Gitea | 외부 git 서버 | `[mirror] DISABLE_NEW_PULL/PUSH` | 꺼짐 (설정함) |
| 리포 마이그레이션 | Gitea | 외부 git 서버 | `[repository] DISABLE_MIGRATIONS` | 꺼짐 |
| 메일 | Gitea | SMTP | `[mailer] ENABLED` | 꺼짐 |
| 버전 확인 | Gitea | gitea.io | `[cron.update_checker] ENABLED` | 꺼짐 |

### 닫힌 것처럼 보이지만 안 닫힌 스위치

이 표를 만들면서 하나 걸렸다. `[picture] DISABLE_GRAVATAR = true`를 app.ini에 써 두면 닫힌
것처럼 보이지만, 이 Gitea 버전은 그 키를 **읽지 않는다** — 관리자 패널로 옮겨졌고 DB에
저장되며, 파일에 있으면 deprecation 오류만 낸다. 다행히 내장 기본값이 꺼짐이라 실제로 열려
있지는 않았지만, "기본값이라 닫혀 있음"과 "정책으로 닫아 둠"은 다르다: 전자는 관리자 패널에서
누가 켜면 아무 흔적 없이 열린다. 그래서 DB에 명시했고, 감사 스크립트는 ini가 아니라 **실효값**을
읽는다. 교훈은 일반적이다 — 스위치를 껐다고 믿지 말고, 실효값을 재라.

## 설계 원칙 — 조용히 나가는 것보다 시끄럽게 거부한다

pip가 대표 사례다. 인덱스가 비어 있을 때 선택지는 둘이다: 공용 PyPI로 조용히 나가거나,
배포를 거부하고 관리자에게 이유를 말하거나. 전자는 성공한 것처럼 보이는 사고이고 후자는
복구 가능한 실패다. 이 플랫폼은 후자를 택한다. AI 스위치도 같다 — 관리자 화면의 토글은
"무엇을 제공하는가"이고 `AI_ENABLED`는 "요청 경로가 존재하는가"다. 관리자 포함 전원에게
적용되는 이유는, 금지된 도구를 켤 수 있는 화면이 있다는 것 자체가 정책 위반이기 때문이다.

## 전사 차단 목록 — 다른 모든 규칙 위 (2026-09-19)

회사가 자기 경계에서 막는 사이트를 이 플랫폼을 타고 넘어가면 안 된다. `apps.yml`
최상위 `blocked:` 목록(`company/blocklist.go`)은 **모든 앱, 모든 모드**(open 포함)에서
브로커가 먼저 거부하는 목적지다 — 앱의 allow 목록이나 승인보다 위. 항목: `host`,
`*.domain`(그 아래 전부), `host:port`, `:port`. 관리자 네트워크 화면에서 추가·제거하고,
변경은 커밋으로 남는다. 거부는 앱의 아웃바운드 로그에 `blocked for every app by the
platform list (…)`로 적힌다. 포트 항목은 아래 포트 규칙과 TCP 터널에도 그대로 적용된다.

## 포트와 TCP — HTTP가 아닌 목적지 (2026-10-19)

허용 규칙의 호스트에는 포트를 붙일 수 있다(`erp.internal:8443`, `ad.company.local:389`;
`ldap://`·`ldaps://`를 붙여 넣으면 389·636). 포트가 없으면 443이다.

- **HTTP 규칙**은 그 포트로 나간다. 앱이 `http://erp.internal:8443/…`을 부르면
  `https://erp.internal:8443/…`이 되고, `erp.internal`(443) 규칙으로는 열리지 않는다.
- **TCP 규칙**(메서드 `TCP`)은 그 host:port로 바이트 파이프 하나를 연다 — LDAP, DB처럼
  HTTP가 아닌 것. 앱이 브로커 소켓에 `CONNECT host:port`를 보내면 브로커가 규칙을 보고
  접속한다(`company/broker_tcp.go`). GET만 허용된 규칙은 터널을 열지 못한다. 터널은
  닫힐 때 `broker.log`에 남고, 앱이 멈추면 끊긴다.

앱 코드는 바꿀 필요가 없다. bubblewrap에서는 앱에 네트워크가 아예 없으므로, 플랫폼 shim이
Python의 소켓 연결을 브로커로 돌린다: 호스트 이름은 이름을 기억하는 자리표시 주소
(198.18.0.0/15)로 풀리고, 그 주소로의 connect가 `CONNECT 이름:포트`가 된다. ldap3 같은
순수 Python 라이브러리는 그대로 동작한다. C로 소켓을 여는 라이브러리(python-ldap 등)는
이 경로를 타지 않는다. 거부되면 앱 로그에 `[platform] outbound TCP to … refused: …`가 한 번
찍힌다.

## 코드가 할 수 없는 것 — 호스트 방화벽

위 표는 **이 프로세스가 스스로 여는** 경로다. 코드가 막을 수 없는 것이 남는다:

- 배포된 앱이 `network.mode: open`으로 승인된 경우의 트래픽 (승인이 곧 정책이다)
- Gitea 밖의 프로세스, 사람이 직접 실행하는 명령
- 코드에 새로 추가되는 호출 (이 표를 갱신하지 않은 것)

그래서 마지막 방어선은 호스트의 **아웃바운드 방화벽**이어야 한다. `check-egress.sh`
3번 항목이 그것을 본다. OUTPUT 체인 기본 정책이 ACCEPT면 호스트는 아무것도 막지 않는
것이고, 위 스위치들이 유일한 방어가 된다 — 그것으로는 부족하다. 권장:

- OUTPUT 기본 DROP, 허용은 사내 인덱스·사내 git·DNS·로컬로 한정
- 사내 이그레스 프록시가 있으면 `[proxy] PROXY_ENABLED = true`, `PROXY_URL`, `PROXY_HOSTS`로
  Gitea의 모든 HTTP를 그리로 보내 프록시 측 허용목록을 한 번 더 거치게

## 검증

```sh
docs/company/scripts/check-egress.sh custom/conf/app.ini
```

1) LISTEN 포트, 2) 지금 살아 있는 외부 연결, 3) 방화벽 OUTPUT 정책, 4) 금지 목적지
(`api.anthropic.com`, `pypi.org`, `gravatar.com` …) 연결 시도 — **전부 실패해야 정상**,
5) 위 표의 설정 키가 실제로 그 값인지. 하나라도 열려 있으면 종료 코드 1.

## 인바운드 — 앱이 무엇이 *될* 수 있는가

밖으로 나가는 경로가 전부라면 절반이다. 코드를 배포하게 해 주는 플랫폼은 반대 질문에도
답해야 한다: 앱이 **자체 서버**가 될 수 있는가. 네트워크가 있는 앱은
`uvicorn --host 0.0.0.0` 한 줄이면 사내망의 누구에게나 닿는, 어떤 접근 정책에도 답하지
않는 서버가 된다.

| 위험 | 통제 | 스위치 | 기본 |
|---|---|---|---|
| 앱이 이그레스 전면 허용(`open`)을 받아 임의 목적지와 통신 | apps.yml 로드·관리자 설정·요청 시 세 지점에서 거부/클램프 | `[company] APP_NETWORK_ALLOW_OPEN` | **금지** |
| 앱이 부서 밖·비로그인에게 노출 | 노출 상한 — 파일에 뭐라 쓰여 있든 요청 시 상한이 이김 | `[company] APP_MAX_ACCESS` (`org`/`login`/`public`) | `public` (기존 동작 유지; 낮추는 건 운영자 결정) |
| 앱이 리스닝 포트를 열어 프록시를 우회 | 샌드박스 netns 안의 소켓 테이블 감시 → 즉시 정지 + 원인 기록 | 자동 (Linux + 샌드박스일 때만 신뢰) | 켜짐 |

리스너 감시는 **네트워크 네임스페이스가 있을 때만** 믿는다. 없으면 `/proc/<pid>/net/tcp`가
호스트 전체의 테이블이라 sshd 때문에 모든 앱을 세우게 되고, 그건 통제를 스스로 불신하게
만드는 오탐이다. 그래서 페이지가 "감시 중인가"를 명시하고, 확신할 수 없는 곳에서는 침묵한다.

상한 둘은 화면의 버튼이 아니라 app.ini다. 클릭 한 번으로 "어떤 앱도 서버가 될 수 없다"를
풀 수 있는 페이지는, 그 정책이 막으려는 것 자체이기 때문이다.

## 감사 — row level

정책이 무엇을 막았는지는 **막힌 요청이 기록될 때만** 안다. 프록시가 내리는 모든 결정 —
통과든 거부든 — 을 앱마다 두 번 쓴다: 메모리 링(최근 수백 건, 관리자 화면이 읽음)과
`logs/access.log`(회전, 재시작에도 남음, 사고 때 grep 하는 것). 한 줄 형식:

```
<RFC3339> <IP> <사용자|-> <메서드> <경로> <상태> <차단사유|->
```

아웃바운드는 브로커가 같은 원칙으로 이미 `broker.log`에 남긴다. 둘 다
`/-/admin/company-network/{owner}/{repo}`에서 최신순으로 본다. 차단된 방문자(IP 또는 계정)
목록과 수동 해제는 `/-/admin/company-network` 상단에 있다.
