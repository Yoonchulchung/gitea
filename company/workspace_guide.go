// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"

	"gitea.dev/modules/translation"
)

// The rules on the editor's empty screen. People paste them into an AI as
// they are, so they are written to work as a prompt, and the copy button
// gives them as plain text rather than whatever the page's markup copies as.
// Only what this server can deploy is listed.

type guideRuntime struct {
	Name, Entry, Detail string
}

type workspaceGuide struct {
	Runtimes []guideRuntime
	Rules    []template.HTML
	Plain    string // what the copy button puts on the clipboard
}

func buildWorkspaceGuide(l translation.Locale, owner, repo string, packagesNeedApproval bool) workspaceGuide {
	tr := l.Tr // escapes the arguments; the markup is our own locale's
	v := supportedRuntimes(owner, repo)
	var g workspaceGuide
	server := v.Python != "" || v.Java != ""

	if v.Python != "" {
		versions := []string{"Python " + v.Python}
		for _, p := range v.Framework {
			versions = append(versions, p.Name+" "+p.Version)
		}
		detail := strings.Join(versions, ", ")
		if len(v.Framework) == 0 {
			detail += " · " + l.TrString("company.workspace.guide.runtime_framework_latest")
		}
		g.Runtimes = append(g.Runtimes, guideRuntime{l.TrString("company.workspace.guide.runtime_python"), "main.py", detail})
		g.Rules = append(g.Rules, tr("company.workspace.guide.rule_python"))
	}
	if v.Java != "" {
		g.Runtimes = append(g.Runtimes, guideRuntime{l.TrString("company.workspace.guide.runtime_java"), "pom.xml", "Java " + v.Java + ", Maven " + v.Maven})
		if packagesNeedApproval {
			g.Rules = append(g.Rules, tr("company.workspace.guide.rule_java", v.Java))
		} else {
			g.Rules = append(g.Rules, tr("company.workspace.guide.rule_java_free", v.Java))
		}
	}
	g.Runtimes = append(g.Runtimes, guideRuntime{l.TrString("company.workspace.guide.runtime_static"), "index.html", l.TrString("company.workspace.guide.runtime_static_note")})
	g.Rules = append(g.Rules, tr("company.workspace.guide.rule_static"))
	if server {
		g.Rules = append(g.Rules, tr("company.workspace.guide.rule_health"), tr("company.workspace.guide.rule_db"), tr("company.workspace.guide.rule_migrations"))
	}
	if v.Python != "" {
		if packagesNeedApproval {
			g.Rules = append(g.Rules, tr("company.workspace.guide.rule_packages", v.Python))
		} else {
			g.Rules = append(g.Rules, tr("company.workspace.guide.rule_packages_free", v.Python))
		}
	}
	g.Rules = append(g.Rules, tr("company.workspace.guide.rule_paths"))
	if server {
		g.Rules = append(g.Rules, tr("company.workspace.guide.rule_network"), tr("company.workspace.guide.rule_secrets"))
	}

	var b strings.Builder
	b.WriteString(l.TrString("company.workspace.guide.rules_title") + "\n")
	b.WriteString(l.TrString("company.workspace.guide.rules_intro") + "\n\n")
	b.WriteString(l.TrString("company.workspace.guide.runtimes_title") + "\n")
	for _, r := range g.Runtimes {
		fmt.Fprintf(&b, "- %s: %s %s. %s\n", r.Name, l.TrString("company.workspace.guide.runtime_entry"), r.Entry, r.Detail)
	}
	b.WriteString("\n" + l.TrString("company.workspace.guide.rules_list") + "\n")
	for i, rule := range g.Rules {
		fmt.Fprintf(&b, "%d. %s\n", i+1, plainFromHTML(string(rule)))
	}
	g.Plain = b.String()
	return g
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// plainFromHTML keeps code as `code`, which an AI reads as code too.
func plainFromHTML(s string) string {
	s = strings.NewReplacer("<code>", "`", "</code>", "`").Replace(s)
	return html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}
