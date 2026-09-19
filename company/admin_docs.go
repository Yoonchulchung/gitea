// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"net/http"

	"gitea.dev/modules/markup"
	"gitea.dev/modules/markup/markdown"
	"gitea.dev/services/context"
)

// AdminDocs is /-/admin/company-docs: the platform documentation the
// assistant works from, listed and readable, with a box to see which parts a
// given question would bring in (company/appdocs.go).
func AdminDocs(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Locale.TrString("company.docs.title")
	ctx.Data["Docs"] = AppDocs()
	if id := ctx.FormString("doc"); id != "" {
		if doc := AppDocByID(id); doc != nil {
			html, err := markdown.RenderString(markup.NewRenderContext(ctx), doc.Body)
			if err != nil {
				ctx.ServerError("RenderString", err)
				return
			}
			ctx.Data["Doc"] = doc
			ctx.Data["DocHTML"] = html
		}
	}
	if q := ctx.FormString("q"); q != "" {
		ctx.Data["Query"] = q
		ctx.Data["Hits"] = SearchAppDocs(q, docsSearchLimit)
		ctx.Data["ContextChars"] = len(AppDocsContext(q))
	}
	ctx.HTML(http.StatusOK, "company/admin_docs")
}
