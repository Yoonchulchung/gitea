# 네트워크 — 외부 통신과 접근 제어
> 앱은 기본적으로 밖에 나갈 수 없다. 허용된 호스트로는 브로커 소켓을 통해서만 나간다. 누가 앱에 들어올 수 있는지는 접근 모드가 정한다.
keywords: 네트워크, network, 외부, outbound, api 호출, requests, httpx, urllib, 브로커, broker, BROKER_SOCKET, 허용목록, allowlist, 차단, blocked, 타임아웃, 접근, access, 로그인, login, org, 공개, public, 사용자, X-Gitea-User, 인증

## 아웃바운드는 기본 차단
- 앱은 기본(`network: none`)으로 DNS, 소켓, HTTP 어느 것도 밖으로 나갈 수 없다. `requests.get("https://...")`는 연결 오류로 실패한다. 샌드박스가 없는 개발 환경에서는 통과할 수 있지만, 허용된 것이 아니다.
- 외부 시스템(사내 ERP, 공공 API 등)이 필요하면 배포 요청 폼의 "외부 접근" 항목에 주소와 메서드(GET만 / 읽기·쓰기)를 적어 요청한다. 관리자가 승인하면 그 앱은 `broker` 모드가 되고, 승인된 호스트·메서드만 허용 목록에 들어간다.
- 회사 전체 차단 목록(관리자 화면 "Blocked for every app")에 있는 호스트·도메인·포트는 어떤 승인보다 우선해서 거부된다.

## 브로커로 나가는 법
- 허용된 앱에는 환경 변수 `BROKER_SOCKET`이 있다. 앱은 이 유닉스 소켓으로 HTTP 요청을 보내고, 플랫폼이 대신 실제 호스트에 HTTPS로 연결한다.
- 코드 예 (httpx 필요, 승인된 패키지여야 함):
  ```python
  import os, httpx
  web = httpx.Client(transport=httpx.HTTPTransport(uds=os.environ["BROKER_SOCKET"]), timeout=30)
  r = web.get("http://erp.internal.company.com/api/v1/employees")
  ```
  URL은 실제 호스트 이름을 쓰되 `http://`로 적는다 (`https://`로 쓰면 소켓 상대로 TLS를 시도해 실패한다). 플랫폼이 밖으로는 https로 연결한다.
- `requests` 라이브러리는 유닉스 소켓을 기본 지원하지 않는다. `httpx`를 쓰거나, `requests-unixsocket` 같은 어댑터가 승인되어 있어야 한다.
- 허용 목록에 없는 호스트나 메서드는 브로커가 403으로 거부하고, 모든 아웃바운드 요청은 앱의 아웃바운드 로그에 남는다. 요청 하나의 상한은 60초다.
- 인증 토큰은 코드에 적지 말고 앱 페이지의 환경 변수에 넣어 `os.environ`으로 읽는다.
- `network: open`(제한 없음)은 관리자 설정(`APP_NETWORK_ALLOW_OPEN`)이 허용할 때만 가능하며, 사실상 쓰지 않는다.

## 인바운드 — 누가 앱을 열 수 있는가
- 접근 모드는 세 가지다. `public`(기본: 서버에 닿는 누구나), `login`(플랫폼에 로그인한 사용자), `org`(앱을 소유한 조직의 구성원과 관리자). 부서가 앱 페이지에서 바꿀 수 있고, 관리자 설정 `APP_MAX_ACCESS`가 상한이다.
- `login`·`org` 모드에서는 플랫폼이 로그인 사용자 헤더(`X-Gitea-User`)에 사용자 이름을 넣어 준다. 앱은 이 값을 화면 표시나 기록에 쓸 수 있다: `request.headers.get("x-gitea-user")`. `public` 모드에서는 이 헤더가 오지 않으며, 앱이 스스로 인증을 구현할 필요는 없다.
- 브라우저가 보낸 `X-Forwarded-*`, `X-Gitea-*` 헤더는 프록시가 모두 제거하므로 위조되지 않는다.
- 앱은 포트를 열어 사내망에 직접 노출될 수 없다. 프록시를 통해서만 접근된다.
