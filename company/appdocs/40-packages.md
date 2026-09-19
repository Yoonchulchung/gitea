# 패키지 — requirements.txt와 승인
> 플랫폼이 기본으로 설치하는 스택, 부서가 추가하는 패키지, 승인 절차와 흔한 빌드 실패.
keywords: 패키지, package, requirements.txt, pip, install, 설치, import, ModuleNotFoundError, 승인, approve, 의존성, dependency, 버전, version, wheel, fastapi, pandas, openpyxl

## 기본 스택
- 모든 앱에 `fastapi`, `uvicorn`, `pydantic`(관리자가 정한 기본 패키지 목록)이 자동으로 설치된다. 이들이 끌어오는 의존성(`starlette`, `anyio`, `h11`, `click`, `typing_extensions` 등)도 함께 설치되며 승인이 필요 없다.
- 기본 패키지를 `requirements.txt`에 다시 적지 않는다. 버전을 고정해 적으면 기본 스택과 충돌해 빌드가 실패할 수 있다.

## 추가 패키지
- 그 밖의 패키지는 저장소 루트의 `requirements.txt`에 한 줄에 하나씩 적는다. `openpyxl` 또는 `openpyxl==3.1.5` 형식. `-r`, `-e`, URL, 로컬 경로는 허용되지 않는다.
- 새 패키지는 관리자의 승인이 필요하다. `requirements.txt`에 적고 배포 요청을 내면 요청 페이지에 "승인이 필요한 패키지"로 표시되고, 관리자가 승인하면서 배포된다. pip가 함께 끌어오는 의존성도 목록에 나타나며 같은 요청에서 승인된다.
- 한 번 승인된 패키지는 그 앱의 허용 목록(`.company/apps.yml`)에 남아 다음 배포부터는 승인 없이 설치된다. 관리자가 철회하면 다음 빌드부터 거부된다.
- 승인 여부는 배포 요청 폼(`/{부서}/{저장소}/deploy`)의 사전 검사와 앱 페이지의 "허용된 패키지" 목록에서 볼 수 있다.

## 버전과 빌드 실패
- 서버의 Python 버전에 맞는 wheel이 있어야 설치된다. 오래된 버전을 고정하면 "No matching distribution found" 로 빌드가 실패한다. 확신이 없으면 버전을 비워 최신 안정 버전을 쓴다.
- 패키지 설치는 배포 때마다 같은 requirements 조합이면 캐시된 환경을 재사용하므로 코드만 바꾼 배포는 빠르다. requirements가 바뀌면 다시 설치한다 (수 분).
- `ModuleNotFoundError: No module named 'x'` 는 (1) `requirements.txt`에 없거나 (2) 아직 승인되지 않았거나 (3) import 이름과 패키지 이름이 다른 경우다. 예: `pip` 이름 `pillow` → import `PIL`, `pyyaml` → `yaml`, `beautifulsoup4` → `bs4`, `scikit-learn` → `sklearn`, `python-dotenv` → `dotenv`.
- 패키지 인덱스는 관리자가 정한 사내 인덱스 또는 PyPI다. 인덱스에 없는 패키지는 설치할 수 없다.
- 시스템 패키지(`apt`)나 컴파일러가 필요한 패키지는 설치되지 않을 수 있다. 순수 Python 또는 wheel이 제공되는 패키지를 고른다.
