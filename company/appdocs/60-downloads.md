# 파일 다운로드 정책
> 앱은 기본적으로 파일을 내려보낼 수 없다. 무엇이 차단되고, 허용을 어떻게 요청하는지.
keywords: 다운로드, download, 엑셀, excel, xlsx, csv, pdf, 첨부, attachment, Content-Disposition, 파일 보내기, export, 내보내기, blob, 5MB, 차단

## 기본은 차단
- 배포 정책 `download.policy`의 기본값은 `block`이다. 프록시가 앱의 응답을 검사해 파일 다운로드로 보이는 것을 막고, 사용자에게 "This app is not allowed to send files. Ask an administrator to approve file downloads for it." 을 보여 준다.
- 차단 기준: (1) `Content-Disposition: attachment` 헤더, (2) 페이지 표시에 필요하지 않은 Content-Type — 허용되는 것은 `text/html`, `text/plain`, `text/css`, `text/csv`, `application/json`, JavaScript, `image/*`, `font/*`, `application/xml`, `text/event-stream` 뿐이고 `application/octet-stream`, `application/vnd.openxmlformats…`(xlsx), `application/pdf`, `application/zip` 은 차단, (3) 응답 크기 5 MB 초과 (`maxResponseBytes`).
- 브라우저 쪽에서 앱의 JavaScript가 만드는 다운로드(`Blob` + `<a download>`, `URL.createObjectURL`)도 정책이 차단일 때는 플랫폼이 주입한 스크립트가 막는다. 우회 코드를 작성하지 않는다.
- 화면에 표시하는 것은 제한이 없다. 표, 차트, `text/csv`를 화면에 텍스트로 보여 주는 것은 된다.

## 허용 받기
- 다운로드가 필요한 앱은 배포 요청 폼(`/{부서}/{저장소}/deploy`)에서 "파일 다운로드 허용" 체크박스를 켜고 이유를 적는다. 관리자가 승인하면 `download.policy: allow`가 되어 위 검사가 적용되지 않는다.
- 허용된 뒤에는 FastAPI에서 `StreamingResponse` 또는 `FileResponse`에 `headers={"Content-Disposition": 'attachment; filename="report.xlsx"'}` 와 올바른 `media_type`을 붙여 돌려준다. 엑셀은 `openpyxl` 같은 패키지 승인이 별도로 필요하다.
- 파일은 `TMPDIR`에 만들거나 메모리(`io.BytesIO`)에서 바로 돌려준다. 앱 디렉터리에는 쓸 수 없다.
