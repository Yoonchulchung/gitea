# 자주 보는 오류와 고치는 법
> 로그와 검사 결과에 나오는 메시지별로, 원인과 코드에서 고칠 것.
keywords: 오류, error, 에러, traceback, exception, 실패, fail, 안돼, 502, 500, 404, NameError, ImportError, ModuleNotFoundError, PermissionError, Read-only, sqlite, OperationalError, 시작 실패, 헬스 실패, 로그

## 시작 단계
- `NameError: name 'X' is not defined` (main.py import 중): 모듈 최상위에서 정의되지 않은 이름을 썼다. 오타이거나 import가 빠졌거나, 테스트용 줄이 남아 있다. 해당 줄을 지우거나 정의를 추가한다.
- `ModuleNotFoundError: No module named 'x'`: 패키지 문서 참고 — `requirements.txt`에 추가하고 승인을 받거나, import 이름을 확인한다. 저장소 안의 다른 파일을 import하는 경우라면 파일이 루트에 있는지, 이름이 맞는지 본다 (작업 디렉터리는 앱 루트).
- `Error loading ASGI app. Attribute "app" not found in module "main"`: `main.py`에 `app = FastAPI()`가 없다. 이름이 `application`이나 `api`면 `app`으로 바꾸거나 `app = api` 한 줄을 추가한다.
- `SyntaxError`: 사전 검사가 줄 번호를 알려 준다. 괄호·따옴표·들여쓰기를 확인한다.
- 시작 검사에서 "startup did not complete": `@app.on_event("startup")` 또는 lifespan 안에서 외부 접속이나 무한 대기를 하고 있다. 시작 코드는 짧고 실패해도 앱이 뜨도록 만든다.
- 헬스 체크 실패(배포 실패, "the app did not respond"): 앱이 30초 안에 응답하지 못했다. import 시점의 무거운 작업(큰 파일 로딩, 모델 로딩)을 첫 요청 시점이나 백그라운드로 옮긴다.

## 실행 중
- `PermissionError` / `OSError: [Errno 30] Read-only file system`: 코드 디렉터리나 서버의 다른 경로에 쓰려 했다. 데이터는 `DB_PATH`의 SQLite로, 임시 파일은 `tempfile`(TMPDIR)로.
- `sqlite3.OperationalError: attempt to write a readonly database` / `unable to open database file`: 경로가 `DATA_DIR` 밖이거나 앱 디렉터리 안이다. `os.environ["DB_PATH"]`를 쓴다.
- `sqlite3.OperationalError: no such table`: 마이그레이션 파일이 없거나 번호가 이어지지 않는다. `migrations/NNN_*.sql`을 추가한다. 앱 코드의 `CREATE TABLE`은 배포 시점에 이미 마이그레이션이 필요한 상태를 만들지 못한다.
- `sqlite3.OperationalError: database is locked`: 연결을 오래 열어 두었거나 트랜잭션을 닫지 않았다. 요청마다 열고 `with`로 닫으며, `timeout=10`을 준다.
- `ConnectionError`, `Name or service not known`, `Network is unreachable`: 외부 접속이 허용되지 않았다. 네트워크 문서 참고 — 승인을 요청하고 `BROKER_SOCKET`을 통해 나간다.
- 브라우저에 "This app is not allowed to send files": 다운로드 정책이 차단이다. 배포 요청에서 다운로드 허용을 요청한다. 화면에 표시하는 것으로 바꿀 수도 있다.
- 페이지 이동 시 404 또는 Gitea 로그인 화면이 뜸: 절대 경로 링크(`/index.html`, `/api/...`) 때문이다. URL 문서 참고 — 상대 경로로 바꾼다.
- 앱이 갑자기 중지되고 앱 페이지에 "메모리 한도"·"CPU 한도": 한도 문서 참고. 데이터를 나눠 처리하고 캐시한다.
- 502 또는 "app is not running": 앱이 죽었거나 중지 상태다. 앱 페이지의 로그에서 마지막 traceback을 보고, 시작/재시작 버튼으로 다시 올린다.
- `RuntimeError: can't start new thread`: 프로세스·스레드 한도(64)를 넘었다. 스레드풀 크기를 줄이거나 작업을 순차로 처리한다.

## 로그 보는 곳
- 앱 페이지(`/_app`)의 로그 탭: `print()`와 uvicorn 로그, traceback. 헬스 체크 요청은 제외된다.
- 배포 요청 페이지: 빌드 실패 이유 (패키지 설치 오류 등).
- 아웃바운드 로그: 브로커가 허용·거부한 외부 요청.
