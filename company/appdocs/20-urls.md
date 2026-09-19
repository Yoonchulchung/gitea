# URL, 경로, 정적 파일
> 앱은 `/apps/{부서}/{저장소}/` 아래에 마운트된다. 링크와 정적 파일 경로가 깨지는 이유와 올바른 작성법.
keywords: url, 경로, 링크, 정적, static, css, js, 이미지, index.html, 404, redirect, root_path, 프리픽스, 페이지 이동

## 앱이 놓이는 주소
- 앱의 주소는 `https://서버/apps/{부서}/{저장소}/` 이고, 앱 코드의 `/`가 이 경로다. 예: `@app.get("/stats")`는 `/apps/PO/Test/stats`에서 열린다.
- uvicorn에 `--root-path`가 전달되므로 FastAPI/Starlette가 스스로 만드는 URL(`url_for`, `/docs`, `request.url_for`)은 프리픽스가 붙어 나온다.

## 링크는 상대 경로로
- HTML이나 JavaScript에 `/index.html`, `/static/app.css`, `/api/items` 처럼 `/`로 시작하는 절대 경로를 적으면 앱이 아니라 Gitea의 `/index.html`을 가리키게 된다. 그 결과가 "입력 화면으로 가면 404" 같은 증상이다.
- 플랫폼의 프록시가 HTML 응답 안의 `href`·`src`·`action` 등에 있는 절대 경로와 `Location` 리다이렉트 헤더에는 프리픽스를 다시 붙여 준다. 하지만 JavaScript 문자열 안의 `fetch("/api/...")`, `window.location = "/..."`, CSS `url(/...)` 은 고쳐 주지 못한다.
- 그래서 규칙은 하나다: **상대 경로를 쓴다.** `fetch("api/items")`, `<a href="stats.html">`, `<link href="static/app.css">`. 하위 경로 페이지에서는 `../`로 올라간다.
- 파이썬 쪽에서 절대 URL이 필요하면 `request.scope["root_path"]` 또는 `os.environ["ROOT_PATH"]`를 앞에 붙인다: `f"{request.scope['root_path']}/api/items"`.
- 페이지가 `/apps/PO/Test` (끝에 슬래시 없음)로 열리면 상대 경로가 한 단계 위를 가리킨다. 링크는 항상 `/apps/PO/Test/` 처럼 슬래시로 끝나는 주소로 안내한다. 프록시가 이 경우를 대부분 보정하지만 링크를 만들 때는 슬래시를 붙인다.

## 정적 파일 제공
- FastAPI에서: `from fastapi.staticfiles import StaticFiles` / `app.mount("/static", StaticFiles(directory="static"), name="static")`. 디렉터리는 저장소 안의 상대 경로다 (작업 디렉터리가 앱 루트).
- `index.html` 같은 페이지를 돌려주려면 `from fastapi.responses import HTMLResponse, FileResponse` 로 파일을 읽어 돌려준다. 파일 경로는 `pathlib.Path(__file__).parent / "index.html"` 처럼 `main.py` 기준으로 만든다.
- Jinja2 템플릿을 쓰려면 `jinja2`가 승인된 패키지여야 한다. 템플릿 안에서도 절대 경로 대신 상대 경로 또는 `request.scope.root_path`를 쓴다.

## 리다이렉트
- `RedirectResponse("/")` 처럼 절대 경로로 리다이렉트하면 프록시가 프리픽스를 붙여 준다. 그래도 `RedirectResponse(url=request.url_for("home"))` 또는 상대 경로 `RedirectResponse("./")` 가 더 안전하다.
- 외부 사이트로의 리다이렉트는 브라우저가 따라가지만, 앱 자체가 외부에 접속하는 것과는 다르다 (네트워크 문서 참고).
