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
| 아바타 | Gitea | gravatar.com | `[picture] DISABLE_GRAVATAR`, `ENABLE_FEDERATED_AVATAR` | 꺼짐 (설정함) |
| 웹훅 | Gitea | 사용자가 입력한 URL | `[webhook] ALLOWED_HOST_LIST` | 빈 목록 (설정함) |
| 미러 | Gitea | 외부 git 서버 | `[mirror] DISABLE_NEW_PULL/PUSH` | 꺼짐 (설정함) |
| 리포 마이그레이션 | Gitea | 외부 git 서버 | `[repository] DISABLE_MIGRATIONS` | 꺼짐 |
| 메일 | Gitea | SMTP | `[mailer] ENABLED` | 꺼짐 |
| 버전 확인 | Gitea | gitea.io | `[cron.update_checker] ENABLED` | 꺼짐 |

## 설계 원칙 — 조용히 나가는 것보다 시끄럽게 거부한다

pip가 대표 사례다. 인덱스가 비어 있을 때 선택지는 둘이다: 공용 PyPI로 조용히 나가거나,
배포를 거부하고 관리자에게 이유를 말하거나. 전자는 성공한 것처럼 보이는 사고이고 후자는
복구 가능한 실패다. 이 플랫폼은 후자를 택한다. AI 스위치도 같다 — 관리자 화면의 토글은
"무엇을 제공하는가"이고 `AI_ENABLED`는 "요청 경로가 존재하는가"다. 관리자 포함 전원에게
적용되는 이유는, 금지된 도구를 켤 수 있는 화면이 있다는 것 자체가 정책 위반이기 때문이다.

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
