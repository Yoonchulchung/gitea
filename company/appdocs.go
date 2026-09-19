// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"embed"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// What an app may do here is a set of rules — one database at DB_PATH,
// relative links, no outbound network, migrations that only add — and the
// assistant that writes department code has to know them or it reproduces
// the same mistakes in every app. The rules live in appdocs/*.md, shipped in
// the binary and listed for administrators (/-/admin/company-docs).
//
// They are not all sent with every request: together they are longer than
// most conversations. Each request gets the sections that match it, found by
// plain lexical search over the sections — words, and character pairs for
// Korean, with a small table joining the two languages — under a size
// budget, plus the contract every app is held to. The model can ask for
// more through the search_platform_docs tool.

//go:embed appdocs/*.md
var appDocsFS embed.FS

// AppDoc is one document.
type AppDoc struct {
	ID       string // the file name without its extension, e.g. "30-data"
	Title    string
	Summary  string
	Keywords []string
	Always   bool // sent with every request, whatever it is about
	Body     string
	Sections []*DocSection
}

// DocSection is one heading of a document, the unit that is retrieved.
type DocSection struct {
	DocID    string
	DocTitle string
	Heading  string
	Body     string
	tf       map[string]int
	length   int
	heading  map[string]bool
}

// Text is the section as the model sees it.
func (s *DocSection) Text() string {
	return "### [" + s.DocTitle + "] " + s.Heading + "\n" + s.Body
}

// DocHit is a section with how well it matched.
type DocHit struct {
	*DocSection
	Score float64
}

const (
	docsContextBudget = 7000 // characters of documentation per request, on top of the contract
	docsSearchLimit   = 6
)

var (
	appDocsOnce     sync.Once
	appDocsList     []*AppDoc
	appDocsSections []*DocSection
	appDocsDF       map[string]int
	appDocsAvgLen   float64
)

// docSynonyms joins the two languages the documents and the questions are
// written in: a question in one reaches the sections in the other. The
// first entry is the tag both sides are given.
var docSynonyms = [][]string{
	{"download", "다운로드", "내려받", "엑셀", "excel", "xlsx", "pdf", "첨부", "attachment", "export", "내보내"},
	{"database", "sqlite", "db_path", "data_dir", "데이터베이스", "디비", "테이블", "table", "migration", "마이그레이션", "스키마", "schema", "sql ", "저장해"},
	{"package", "패키지", "requirements", "pip", "install", "설치", "modulenotfound", "모듈", "module", "라이브러리", "library", "import"},
	{"network", "네트워크", "외부", "outbound", "httpx", "requests", "api 호출", "broker", "브로커", "호출", "연결", "connection"},
	{"limit", "메모리", "memory", "cpu", "한도", "제한", "느려", "느림", "killed", "oom", "용량"},
	{"deploy", "배포", "승인", "approve", "요청", "request", "머지", "merge", "release", "rollback", "롤백", "미리보기", "preview", "커밋", "commit"},
	{"url", "경로", "링크", "link", "redirect", "리다이렉트", "static", "정적", "css", "이미지", "image", "404", "root_path", "prefix", "index.html", "페이지 이동", "화면으로"},
	{"error", "에러", "오류", "traceback", "exception", "실패", "fail", "안돼", "안 돼", "안됨", "안 됨", "되지 않", "깨져", "500", "502"},
	{"startup", "health", "헬스", "시작", "실행", "start", "uvicorn", "main.py", "뜨지"},
	{"access", "접근", "로그인", "login", "권한", "인증", "사용자", "user", "공개", "public", "org", "부서원"},
	{"env", "환경변수", "환경 변수", "environ", "secret", "비밀", "token", "토큰", "api 키", "api key"},
	{"state", "상태", "캐시", "cache", "재시작", "restart", "crash", "죽"},
	{"file", "파일", "업로드", "upload", "tmp", "임시", "readonly", "read-only", "permissionerror"},
}

// docTokens breaks text into what is searched: lowercase words, and for
// Korean — which has no spaces between the parts of a word — every pair of
// adjacent characters, so "다운로드를" still meets "다운로드".
func docTokens(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var word, hangul []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushHangul := func() {
		switch {
		case len(hangul) == 0:
		case len(hangul) <= 2:
			out = append(out, string(hangul))
		default:
			for i := 0; i+1 < len(hangul); i++ {
				out = append(out, string(hangul[i:i+2]))
			}
		}
		hangul = hangul[:0]
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Hangul, r):
			flushWord()
			hangul = append(hangul, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			flushHangul()
			word = append(word, r)
		default:
			flushWord()
			flushHangul()
		}
	}
	flushWord()
	flushHangul()
	for _, syn := range docSynonyms {
		for _, term := range syn {
			if strings.Contains(s, term) {
				out = append(out, "~"+syn[0])
				break
			}
		}
	}
	return out
}

func loadAppDocs() {
	entries, err := appDocsFS.ReadDir("appdocs")
	if err != nil {
		return
	}
	appDocsDF = map[string]int{}
	total := 0
	for _, e := range entries {
		body, err := appDocsFS.ReadFile(path.Join("appdocs", e.Name()))
		if err != nil {
			continue
		}
		doc := parseAppDoc(strings.TrimSuffix(e.Name(), ".md"), string(body))
		appDocsList = append(appDocsList, doc)
		for _, s := range doc.Sections {
			s.tf = map[string]int{}
			for _, t := range docTokens(s.Heading + "\n" + s.Body) {
				s.tf[t]++
				s.length++
			}
			s.heading = map[string]bool{}
			for _, t := range docTokens(s.Heading) {
				s.heading[t] = true
			}
			for t := range s.tf {
				appDocsDF[t]++
			}
			total += s.length
			appDocsSections = append(appDocsSections, s)
		}
	}
	sort.Slice(appDocsList, func(i, j int) bool { return appDocsList[i].ID < appDocsList[j].ID })
	if len(appDocsSections) > 0 {
		appDocsAvgLen = float64(total) / float64(len(appDocsSections))
	}
}

// parseAppDoc reads the head lines — title, summary, keywords, always —
// and splits the rest at its "## " headings.
func parseAppDoc(id, body string) *AppDoc {
	doc := &AppDoc{ID: id, Body: body}
	var current *DocSection
	var text strings.Builder
	closeSection := func() {
		if current != nil {
			current.Body = strings.TrimSpace(text.String())
			if current.Body != "" {
				doc.Sections = append(doc.Sections, current)
			}
		}
		text.Reset()
	}
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case doc.Title == "" && strings.HasPrefix(line, "# "):
			doc.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			continue
		case current == nil && strings.HasPrefix(line, "> "):
			doc.Summary = strings.TrimSpace(strings.TrimPrefix(line, "> "))
			continue
		case current == nil && strings.HasPrefix(line, "keywords:"):
			for k := range strings.SplitSeq(strings.TrimPrefix(line, "keywords:"), ",") {
				if k = strings.TrimSpace(k); k != "" {
					doc.Keywords = append(doc.Keywords, k)
				}
			}
			continue
		case current == nil && strings.HasPrefix(line, "always:"):
			doc.Always = strings.TrimSpace(strings.TrimPrefix(line, "always:")) == "yes"
			continue
		case strings.HasPrefix(line, "## "):
			closeSection()
			current = &DocSection{DocID: id, DocTitle: doc.Title, Heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))}
			continue
		}
		if current == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			current = &DocSection{DocID: id, DocTitle: doc.Title, Heading: doc.Title}
		}
		text.WriteString(line)
		text.WriteString("\n")
	}
	closeSection()
	return doc
}

// AppDocs is every document, in order.
func AppDocs() []*AppDoc {
	appDocsOnce.Do(loadAppDocs)
	return appDocsList
}

// AppDocByID finds one document.
func AppDocByID(id string) *AppDoc {
	for _, d := range AppDocs() {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// SearchAppDocs ranks sections against a question: BM25 over the tokens,
// with a heading that matches counting extra.
func SearchAppDocs(query string, limit int) []DocHit {
	appDocsOnce.Do(loadAppDocs)
	terms := map[string]bool{}
	for _, t := range docTokens(query) {
		terms[t] = true
	}
	if len(terms) == 0 || len(appDocsSections) == 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(appDocsSections))
	var hits []DocHit
	for _, s := range appDocsSections {
		score := 0.0
		for t := range terms {
			df := appDocsDF[t]
			if df == 0 {
				continue
			}
			tf := float64(s.tf[t])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df)+0.5)/(float64(df)+0.5))
			score += idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*float64(s.length)/appDocsAvgLen))
			if s.heading[t] {
				score += idf
			}
		}
		if score > 0 {
			hits = append(hits, DocHit{DocSection: s, Score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// AppDocsContext is what goes into a prompt for one request: the contract,
// then the best-matching sections while the budget lasts.
func AppDocsContext(query string) string {
	appDocsOnce.Do(loadAppDocs)
	var b strings.Builder
	b.WriteString("\n\n## Platform rules (the parts relevant to this request)\n" +
		"This repository runs on the company's internal app platform. Code you write must follow these rules; " +
		"where the employee's request conflicts with them, explain the rule and do it the platform's way. " +
		"Use search_platform_docs for anything not covered below.\n\n")
	included := map[*DocSection]bool{}
	for _, d := range AppDocs() {
		if !d.Always {
			continue
		}
		for _, s := range d.Sections {
			b.WriteString(s.Text())
			b.WriteString("\n\n")
			included[s] = true
		}
	}
	used := 0
	for _, h := range SearchAppDocs(query, docsSearchLimit*2) {
		if included[h.DocSection] {
			continue
		}
		text := h.Text()
		if used+len(text) > docsContextBudget {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n\n")
		included[h.DocSection] = true
		used += len(text)
	}
	return b.String()
}

// AppDocsSearchText is the search_platform_docs tool's answer.
func AppDocsSearchText(query string) string {
	hits := SearchAppDocs(query, docsSearchLimit)
	if len(hits) == 0 {
		return "Nothing in the platform documentation matches that. Documents: " + docTitles()
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.Text())
		b.WriteString("\n\n")
	}
	return b.String()
}

func docTitles() string {
	titles := make([]string, 0, len(AppDocs()))
	for _, d := range AppDocs() {
		titles = append(titles, d.Title)
	}
	return strings.Join(titles, "; ")
}
