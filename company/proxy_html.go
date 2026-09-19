// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// An app mounted at /apps/{owner}/{repo} still writes `href="/docs"`, and that
// link goes to Gitea rather than to the app. --root-path fixes the URLs the
// framework builds; it cannot fix one typed into a template.
//
// This is not something a department can be asked to get right. The prefix is
// the platform's choice — it exists because there is no per-app hostname to
// give them — and a non-developer writing a page has no reason to know that
// "/docs" and "docs" differ. Their app works when they run it locally and
// breaks here, which reads as the platform being broken.
//
// So the proxy rewrites root-relative URLs in HTML on the way out. Bounded on
// purpose:
//
//   - text/html only, and only when the app did not compress it. Rewriting a
//     body we would have to decode first trades a link for a class of bugs
//     that corrupt whole pages.
//   - A fixed set of URL attributes. Not a search-and-replace over the text:
//     "/docs" inside a paragraph is prose, and changing it would be a bug.
//   - Left alone: absolute URLs, protocol-relative URLs, fragments, and
//     anything with a scheme (mailto:, data:, javascript:).
//
// What it does not reach, honestly: a URL a script builds at runtime
// (fetch("/api/items")), and url(/x) inside CSS. Those need the app to use a
// relative path, which is what the AI assistant is now told to write
// (company/platformenv.go).

// unreachableHosts are hostnames that cannot mean anything in a visitor's
// browser: the placeholder this proxy hands the transport, and the addresses a
// developer writes when a service is "here".
//
// An app that hardcodes http://0.0.0.0:3000/items or http://localhost:8000/x
// is naming itself as it was reachable somewhere else. Following such a link
// from a browser reaches nothing at all — 0.0.0.0 is not an address you
// connect to, and a loopback address is the *viewer's* own machine. So the
// path is taken and the host discarded, which is the only reading that can
// ever work.
var unreachableHosts = map[string]bool{
	internalAppHost: true,
	"localhost":     true,
	"127.0.0.1":     true,
	"0.0.0.0":       true,
	"::1":           true,
	"[::1]":         true,
}

// localPath reduces a URL that names this host, or a host no browser can
// reach, to its path. Reports false for anything genuinely elsewhere.
//
// sameHost is the host the visitor used. An app building an absolute URL from
// the Host header it was given produces exactly that, and it is the same page
// either way — but as a path it survives the visitor reaching this instance by
// a different name.
func localPath(value, sameHost string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false // mailto:, data:, javascript: — not a location
	}
	host := parsed.Hostname()
	if !unreachableHosts[strings.ToLower(host)] &&
		!strings.EqualFold(parsed.Host, sameHost) && !strings.EqualFold(host, hostWithoutPort(sameHost)) {
		return "", false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		path += "#" + parsed.EscapedFragment()
	}
	return path, true
}

func hostWithoutPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// rewrittenURLAttrs are the attributes whose value is a single URL.
var rewrittenURLAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true,
	"poster": true, "data": true, "ping": true,
}

// htmlRewriteLimit caps what will be buffered to rewrite. A page past this is
// passed through untouched rather than held in memory: the download policy
// already bounds responses, and a link is not worth the risk of holding a very
// large body.
const htmlRewriteLimit = 8 << 20

// rewriteHTMLBody prefixes root-relative URLs in an HTML response.
//
// Returns without touching anything it is not sure about, because passing the
// body through unchanged leaves one broken link, while getting this wrong
// breaks the page.
func rewriteHTMLBody(prefix string, resp *http.Response, shim, downloadsBlocked bool) {
	sameHost := ""
	if resp.Request != nil {
		sameHost = resp.Request.Host
	}
	if resp.Body == nil || !isHTMLResponse(resp) {
		return
	}
	// A compressed body would have to be decoded, rewritten and re-encoded.
	// Skipped rather than attempted: identity is the normal case for these
	// apps, and the failure mode of the alternative is a corrupted page.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return
	}
	if resp.ContentLength > htmlRewriteLimit {
		return
	}

	original := resp.Body
	body, err := io.ReadAll(io.LimitReader(original, htmlRewriteLimit+1))
	if err != nil || len(body) > htmlRewriteLimit {
		// Too big, or broken partway: served whole and unrewritten, the read
		// part first and the rest still streaming behind it. A page that could
		// not be rewritten is better than a page cut off at the limit.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), original), original}
		return
	}
	_ = original.Close()

	rewritten := rewriteHTML(prefix, sameHost, body)
	// text/html only: in XHTML a script is parsed as markup, and the shim's
	// own text would end the page in a parse error.
	if shim && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		rewritten = injectAppPathShim(prefix, rewritten, downloadsBlocked)
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
}

// appPathShimJS sends a page script's root-relative requests to the app. The
// proxy rewrites URLs in markup, but fetch("/api/entries") is a string inside
// a script that only the page can see, so an app written to run at the root
// reached Gitea instead, got its login page back, and told its user it could
// not reach its own server.
const appPathShimJS = `(function () {
  if (window.__appPathShim) return;
  window.__appPathShim = true;
  var prefix = PREFIX, origin = location.origin;
  if (DOWNLOADS_BLOCKED) {
    // A file the page builds for itself never passes the proxy, so the
    // download policy is applied where it happens: the click that saves it.
    var deny = function () { window.alert(DOWNLOAD_MESSAGE); };
    var click = HTMLAnchorElement.prototype.click;
    HTMLAnchorElement.prototype.click = function () {
      if (this.hasAttribute("download")) { deny(); return; }
      return click.apply(this, arguments);
    };
    document.addEventListener("click", function (e) {
      var a = e.target && e.target.closest ? e.target.closest("a[download]") : null;
      if (a) { e.preventDefault(); e.stopImmediatePropagation(); deny(); }
    }, true);
  }
  function mounted(p) {
    return p === prefix || [prefix + "/", prefix + "?", prefix + "#"].some(function (s) { return p.indexOf(s) === 0; });
  }
  function fix(u) {
    if (u.indexOf(origin + "/") === 0) return origin + fix(u.slice(origin.length));
    if (u.charAt(0) !== "/" || u.charAt(1) === "/" || mounted(u)) return u;
    return prefix + u;
  }
  function fixSocket(u) {
    u = String(u);
    var ws = location.protocol === "https:" ? "wss://" : "ws://";
    if (u.indexOf(ws + location.host + "/") === 0) return ws + location.host + fix(u.slice(ws.length + location.host.length));
    return fix(u);
  }
  var fetch0 = window.fetch;
  if (fetch0) {
    window.fetch = function (input, init) {
      if (typeof input === "string" || input instanceof URL) {
        return fetch0.call(window, fix(String(input)), init);
      }
      if (!(input instanceof Request) || fix(input.url) === input.url) return fetch0.call(window, input, init);
      var r = input, bodyless = r.method === "GET" || r.method === "HEAD";
      return (bodyless ? Promise.resolve(undefined) : r.arrayBuffer()).then(function (body) {
        return fetch0.call(window, new Request(fix(r.url), {
          method: r.method, headers: r.headers, body: body, mode: r.mode, credentials: r.credentials,
          cache: r.cache, redirect: r.redirect, referrer: r.referrer, integrity: r.integrity,
          keepalive: r.keepalive, signal: r.signal
        }), init);
      });
    };
  }
  var open0 = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url) {
    var args = Array.prototype.slice.call(arguments);
    args[1] = fix(String(url));
    return open0.apply(this, args);
  };
  if (navigator.sendBeacon) {
    var beacon0 = navigator.sendBeacon;
    navigator.sendBeacon = function (url, data) { return beacon0.call(navigator, fix(String(url)), data); };
  }
  ["EventSource", "WebSocket"].forEach(function (name) {
    var Ctor = window[name];
    if (!Ctor) return;
    var Wrapped = function (url, arg) {
      return arg === undefined ? new Ctor(fixSocket(url)) : new Ctor(fixSocket(url), arg);
    };
    Wrapped.prototype = Ctor.prototype;
    Object.keys(Ctor).forEach(function (k) { Wrapped[k] = Ctor[k]; });
    window[name] = Wrapped;
  });
})();`

// injectAppPathShim puts appPathShimJS first in the document, before any of
// the app's own scripts run. Only into a whole document: a fragment fetched
// to be swapped into a page has to arrive exactly as the app sent it.
func injectAppPathShim(prefix string, body []byte, downloadsBlocked bool) []byte {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	offset, at := 0, -1
	for tt := tokenizer.Next(); tt != html.ErrorToken; tt = tokenizer.Next() {
		offset += len(tokenizer.Raw())
		if tt != html.StartTagToken {
			continue
		}
		name, _ := tokenizer.TagName()
		if string(name) == "html" {
			at = offset // a <head> may still follow; right after it is better
			continue
		}
		if string(name) == "head" {
			at = offset
		}
		break
	}
	if at < 0 {
		return body
	}
	js := strings.Replace(appPathShimJS, "PREFIX", strconv.Quote(prefix), 1)
	js = strings.Replace(js, "DOWNLOADS_BLOCKED", strconv.FormatBool(downloadsBlocked), 1)
	js = strings.Replace(js, "DOWNLOAD_MESSAGE", strconv.Quote(platformLocale().TrString("company.appgate.downloads_blocked")), 1)
	script := "<script>" + js + "</script>"
	out := make([]byte, 0, len(body)+len(script))
	out = append(out, body[:at]...)
	out = append(out, script...)
	return append(out, body[at:]...)
}

func isHTMLResponse(resp *http.Response) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	return ct == "text/html" || ct == "application/xhtml+xml"
}

// rewriteHTML walks the document and rewrites URL attributes.
//
// Tokenised rather than pattern-matched: an attribute is only an attribute
// where the parser says it is, so this cannot rewrite a path that happens to
// appear in body text or inside a script.
func rewriteHTML(prefix, sameHost string, body []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(body) + len(body)/16)

	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() != io.EOF {
				// Malformed markup: emit what is left verbatim rather than
				// truncating the page at the point the parser gave up.
				out.Write(tokenizer.Raw())
				out.Write(tokenizer.Buffered())
			}
			return out.Bytes()

		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			changed := false
			for i, attr := range token.Attr {
				var rewritten string
				switch {
				case attr.Key == "srcset":
					rewritten = prefixSrcset(prefix, sameHost, attr.Val)
				case rewrittenURLAttrs[attr.Key]:
					rewritten = prefixDocumentURLFor(prefix, sameHost, attr.Val)
				default:
					continue
				}
				if rewritten != attr.Val {
					token.Attr[i].Val = rewritten
					changed = true
				}
			}
			if changed {
				out.WriteString(token.String())
			} else {
				// Unchanged tags are copied byte for byte. Re-rendering every
				// token would normalise quoting and attribute order across the
				// whole document, which makes every page a diff against itself
				// and any real change impossible to spot.
				out.Write(tokenizer.Raw())
			}

		default:
			out.Write(tokenizer.Raw())
		}
	}
}

// prefixDocumentURL prefixes one root-relative URL.
//
// Deliberately narrow. A value is rewritten only when it starts with a single
// "/", which is exactly the case that is wrong under a mount prefix.
//
// One case cannot be decided from the string alone: a value that already
// starts with the prefix. "/apps/PO/app/items" is either the framework's own
// output for the app's /items page, or an app that serves a page literally at
// that path and wants it prefixed again. Nothing in the URL distinguishes
// them, and the app cannot be asked without making a request with side
// effects.
//
// It is read as already-mounted, because that is what --root-path produces
// for every URL a framework builds and it is therefore almost all of them.
// The other reading requires someone to have typed the platform's own mount
// path into their template — the exact thing they are told not to do, and
// something a non-developer has no reason to invent. Matching is
// case-sensitive and stops at a segment boundary, so a different app whose
// name merely starts the same ("/apps/PO/app-two") is not mistaken for this
// one.
func prefixDocumentURL(prefix, value string) string {
	return prefixDocumentURLFor(prefix, "", value)
}

// prefixDocumentURLFor is the same with the visitor's host known, so an
// absolute URL naming this instance is recognised as a local path rather than
// left alone.
func prefixDocumentURLFor(prefix, sameHost, value string) string {
	trimmed := strings.TrimSpace(value)
	if path, ok := localPath(trimmed, sameHost); ok {
		return prefixDocumentURLFor(prefix, "", path)
	}
	if !strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "//") {
		return value // relative (already correct), or protocol-relative and off-host
	}
	if trimmed == prefix || strings.HasPrefix(trimmed, prefix+"/") {
		return value // already mounted — what --root-path produces
	}
	return prefix + trimmed
}

// prefixSrcset rewrites the URL in each candidate of a srcset, leaving the
// descriptors ("2x", "640w") alone.
func prefixSrcset(prefix, sameHost, value string) string {
	parts := strings.Split(value, ",")
	for i, part := range parts {
		lead := part[:len(part)-len(strings.TrimLeft(part, " \t\n\r\f"))]
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		fields[0] = prefixDocumentURLFor(prefix, sameHost, fields[0])
		parts[i] = lead + strings.Join(fields, " ")
	}
	return strings.Join(parts, ",")
}
