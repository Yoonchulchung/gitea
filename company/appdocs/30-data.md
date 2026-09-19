# 데이터 저장 — SQLite, 마이그레이션, 상태
> 앱이 저장할 수 있는 곳은 `DB_PATH`의 SQLite 데이터베이스 하나다. 스키마는 마이그레이션 파일로, 임시 상태는 `platform.state`로.
keywords: sqlite, database, DB_PATH, DATA_DIR, 데이터베이스, 테이블, 저장, 마이그레이션, migration, schema, 스키마, 파일 업로드, blob, 용량, 80%, 스냅샷, 백업, state, 상태, 캐시, WAL, readonly

## 데이터베이스는 하나, 경로는 DB_PATH
- 앱은 영속 데이터를 **SQLite 데이터베이스 하나**에 저장한다. 경로는 환경 변수 `DB_PATH`. 항상 `sqlite3.connect(os.environ["DB_PATH"], timeout=10)` 으로 연다.
- `sqlite3.connect("data.db")` 처럼 다른 경로를 열어도 플랫폼이 `DATA_DIR` 안으로 돌려 준다 (`_company_platform` 심이 sqlite3에만 적용). 하지만 코드에는 `DB_PATH`를 명시하는 것이 맞다.
- 앱 디렉터리는 읽기 전용이므로 `data.json`, `records.csv` 같은 파일을 코드 옆에 쓰는 방식은 `Read-only file system` 으로 실패한다. 파일로 저장하던 것은 테이블로 옮긴다.
- `DATA_DIR` 안에 다른 파일을 두는 것은 가능하지만 용량 한도와 스냅샷 대상에 포함되며, 데이터베이스 밖의 파일은 관리 화면에서 보이지 않는다. 업로드 파일은 테이블의 BLOB 컬럼에 넣는다.
- 저널 모드(WAL)는 플랫폼이 설정한다. `PRAGMA journal_mode`를 앱에서 바꾸지 않는다.
- 동기 엔드포인트는 스레드풀에서 돌기 때문에 sqlite3 연결을 전역으로 공유하면 `ProgrammingError: SQLite objects created in a thread…` 가 난다. 요청마다 열고 닫는다 (`with sqlite3.connect(...) as conn:` 또는 의존성 함수).

## 스키마는 마이그레이션 파일로
- 앱 코드에서 `CREATE TABLE`을 실행하지 않는다 (import 시점의 `init_db()` 포함). 스키마는 저장소의 `migrations/001_create_records.sql`, `migrations/002_add_note.sql` 처럼 번호가 붙은 SQL 파일에 두고, 플랫폼이 배포 때 옛 앱을 멈추기 전에 순서대로 적용한다.
- 파일 이름은 `NNN_설명.sql` (3자리 이상 0채움, 오름차순). 각 파일은 한 트랜잭션으로 적용되고 `_schema_migrations` 테이블에 기록된다.
- **추가만 허용(additive only)**: 테이블 추가, NULL 허용 컬럼 추가, DEFAULT가 있는 컬럼 추가. `DROP TABLE`, `DROP COLUMN`, `RENAME`, DEFAULT 없는 `NOT NULL` 컬럼은 기계적으로 거부된다. 어떤 과거 버전으로도 되돌릴 수 있어야 하기 때문이다. 정말 필요하면 파일 첫 줄에 `-- platform: destructive`를 적고, 배포 요청에서 관리자 승인을 받는다.
- 이미 적용된 마이그레이션 파일은 수정하지 않는다 (체크섬이 달라지면 배포 거부). 바꿀 것이 있으면 다음 번호 파일을 추가한다.
- `CREATE TABLE IF NOT EXISTS`를 마이그레이션 안에서 쓰는 것은 괜찮다.

## 용량
- 앱 데이터 한도는 기본 128 MB (관리자가 앱별로 조정). 80%를 넘으면 앱 페이지와 관리 화면에 경고가 뜨고, 100%가 되면 플랫폼이 앱을 중지시킨다. 더 필요하면 배포 요청 폼의 "데이터 저장 공간" 항목으로 상향을 요청한다.
- 오래된 행을 지우거나 BLOB을 줄이는 것이 먼저다. `VACUUM`은 앱에서 실행하지 않는다 (플랫폼이 관리).
- 플랫폼이 배포 전과 정기적으로 스냅샷을 찍고, 관리 화면에서 복원할 수 있다. 앱이 자체 백업을 만들 필요는 없다.
- 앱을 플랫폼에서 내려도 데이터는 보존 기간 동안 남아 있다가 다시 배포하면 그대로 돌아온다.

## 메모리 안의 상태 — platform.state
- 앱은 한도 초과, 서버 메모리 부족, 배포 때문에 언제든 중지될 수 있다. 프로세스 메모리는 보존되지 않는다.
- 다시 시작해도 남겨야 하는 작은 값(캐시, 마지막 처리 시각 등)은 `import _company_platform as platform` 뒤에 `platform.state["key"] = value`로 넣는다. JSON으로 표현 가능한 값만 된다. 정상 종료 시와 30초마다 `DATA_DIR/platform-state.json`에 저장되고, 시작 시 앱 코드보다 먼저 복원된다.
- 사용자 데이터는 여기가 아니라 데이터베이스에 넣는다. `state`는 비어 있어도 앱이 동작해야 한다.
