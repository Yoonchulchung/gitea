# 앱 계약 — 플랫폼이 앱에 기대하는 것
> 부서 앱이 이 플랫폼에서 실행되기 위한 최소 규칙: 파일 구조, 시작 명령, 환경 변수, 헬스 체크.
keywords: main.py, app, uvicorn, FastAPI, health, 시작, 실행, 환경변수, 구조, 계약, 파일
always: yes

## 저장소 구조와 시작 명령
- 저장소 루트에 `main.py`가 있어야 하고, 그 안에 ASGI 앱 객체 `app`이 있어야 한다. 보통 `from fastapi import FastAPI` / `app = FastAPI()`.
- 플랫폼은 앱을 항상 `uvicorn main:app --uds ${SOCKET} --root-path ${ROOT_PATH}` 로 시작한다. 시작 명령은 부서가 바꿀 수 없다. `if __name__ == "__main__": uvicorn.run(...)` 블록은 있어도 무시된다.
- 포트를 열지 않는다. 앱은 유닉스 소켓으로만 서비스되고, 플랫폼의 프록시가 `/apps/{부서}/{저장소}/` 에서 그 소켓으로 연결한다. `--host 0.0.0.0` 같은 설정은 의미가 없다.
- `main.py`가 import 되는 순간 실행되는 코드(모듈 최상위)에서 예외가 나면 앱은 시작하지 못한다. 정의되지 않은 이름, 없는 모듈, 파일 열기 실패가 대표적이다.
- 코드 디렉터리(`/app`)는 읽기 전용이다. 앱이 자기 파일 옆에 무언가를 쓰면 `PermissionError` 또는 `OSError: Read-only file system`이 난다. 쓰기는 `DATA_DIR`(영속)과 `TMPDIR`(임시)에서만 된다.
- 심볼릭 링크는 배포되지 않고, 파일은 실행 권한 없이 배포된다. 전체 소스 크기 상한은 64 MB다.

## 헬스 체크
- 배포와 재시작 뒤 플랫폼은 `/health` (apps.yml의 `healthPath`)를 GET 한다. 5xx가 아니면 살아 있는 것으로 본다. 404도 통과하지만, `@app.get("/health")` 로 `{"status": "ok"}` 를 돌려주는 것이 바람직하다.
- 시작 후 약 30초 안에 응답이 없으면 배포는 실패로 기록되고 이전 버전이 계속 서비스된다.
- 실행 중에도 30초마다 같은 경로를 확인한다. 약 2분간 응답이 없으면 자동으로 재시작하고, 15분 안에 3번 재시작하면 멈춘다. 시작 직후 1분은 확인하지 않는다.
- 헬스 체크 요청에는 `platform-health-check=1` 쿼리가 붙고 로그에서 제외된다.

## 환경 변수
플랫폼이 앱 프로세스에 주는 값. Gitea 자체의 환경은 전혀 상속되지 않는다.
- `SOCKET`: 앱이 바인드하는 유닉스 소켓 (uvicorn이 사용). 직접 쓸 일 없음.
- `ROOT_PATH`: 앱이 마운트된 경로 `/apps/{부서}/{저장소}`. uvicorn `--root-path`로도 전달된다.
- `DATA_DIR`, `DB_PATH`: 영속 데이터 디렉터리와 그 안의 SQLite 파일 경로 (데이터 문서 참고).
- `HOME`, `TMPDIR`: 앱 전용 임시 디렉터리. `TMPDIR`은 앱이 시작될 때마다 비워진다. `/tmp`를 직접 쓰지 말고 `tempfile` 모듈을 쓴다 (TMPDIR을 따른다).
- `BROKER_SOCKET`: 외부 통신이 허용된 앱에만 있다 (네트워크 문서 참고).
- 부서가 앱 페이지(`/{부서}/{저장소}/_app`)에서 직접 추가한 환경 변수. 이름은 `[A-Za-z_][A-Za-z0-9_]*`, `PATH`·`HOME`·`PYTHON*`·`SOCKET`·`ROOT_PATH`·`DB_PATH` 같은 플랫폼 예약 이름은 거부된다. API 키 같은 비밀은 코드에 적지 말고 여기에 넣고 `os.environ["이름"]`으로 읽는다.
- `PYTHONDONTWRITEBYTECODE=1`, `PYTHONUNBUFFERED=1`이 설정된다. `print()`는 즉시 앱 로그에 남는다.

## 프로세스와 격리
- 앱은 샌드박스(운영 서버에서는 bubblewrap 또는 Landlock) 안에서 Gitea의 자식 프로세스로 돈다. 다른 앱, Gitea 데이터, 서버의 다른 파일은 보이지 않는다.
- Landlock 환경에서는 `/proc`, `/tmp`, 홈 디렉터리가 없다. 앱 디렉터리·`DATA_DIR`·`TMPDIR`·시스템 라이브러리 밖의 경로를 열면 `PermissionError`가 난다. 이는 앱 버그가 아니라 격리 규칙이다.
- `subprocess`로 다른 프로그램을 실행할 수는 있지만 같은 샌드박스 안에서 돌고, 프로세스 수 상한(기본 64)에 포함된다.
- 앱이 죽으면 플랫폼이 다시 시작한다. 연속으로 여러 번 죽으면 자동 재시작을 멈추고 앱 페이지에 이유를 표시한다.
