// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// What the platform AI cost. Every call it makes is one line in a monthly
// file, so the AI page can say what was spent on what without a table of its
// own. Personal AI keys are their owners' bill and are not recorded here.

// aiUsage is the token counts a provider reported for one reply.
type aiUsage struct {
	Input, Output int
}

// aiUsageEntry is one platform AI call.
type aiUsageEntry struct {
	At      int64  `json:"at"`
	Purpose string `json:"purpose"` // what it was for, as the page shows it
	Model   string `json:"model"`
	Input   int    `json:"in"`
	Output  int    `json:"out"`
	Actor   string `json:"actor,omitempty"`
	Ref     string `json:"ref,omitempty"` // which request or app it was about
	// Cost is USD at that model's price when the call was made, so switching
	// models or prices later does not rewrite what was spent. Missing on
	// lines written before it was recorded; those are priced by their model.
	Cost *float64 `json:"cost,omitempty"`
}

// Purposes, as the usage table labels them.
const (
	aiPurposeDeployReview  = "배포 검토"
	aiPurposeCodeReview    = "코드 리뷰"
	aiPurposeReviewChat    = "검토 채팅"
	aiPurposeTest          = "연결 테스트"
	aiPurposePriceEstimate = "단가 추정"
)

var (
	aiUsageMu     sync.Mutex
	aiUsageMonthR = regexp.MustCompile(`^\d{4}-\d{2}$`)
)

func aiUsageDir() string { return filepath.Join(setting.AppDataPath, "company-ai-usage") }

// recordAIUsage appends one call. Best-effort: losing a line of accounting
// must never fail the review it accounts for.
func recordAIUsage(ctx context.Context, purpose, model, actor, ref string, u aiUsage) {
	if u.Input == 0 && u.Output == 0 {
		return // the provider reported nothing; a zero line says nothing either
	}
	now := time.Now()
	entry := aiUsageEntry{At: now.Unix(), Purpose: purpose, Model: model, Input: u.Input, Output: u.Output, Actor: actor, Ref: ref}
	if platformRunning.Load() { // no database behind the prices in tests
		if p, ok := modelPrices(ctx)(model); ok {
			cost := tokenCost(u.Input, u.Output, p.In, p.Out)
			entry.Cost = &cost
		} else {
			ensureModelPrice(ctx, model) // priced from the next report on
		}
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	aiUsageMu.Lock()
	defer aiUsageMu.Unlock()
	if err := os.MkdirAll(aiUsageDir(), 0o700); err != nil {
		log.Error("company: AI usage: %v", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(aiUsageDir(), now.Format("2006-01")+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Error("company: AI usage: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Error("company: AI usage: %v", err)
	}
}

// loadAIUsage reads one month ("2006-01"); an unknown month is empty.
func loadAIUsage(month string) []aiUsageEntry {
	if !aiUsageMonthR.MatchString(month) {
		return nil
	}
	aiUsageMu.Lock()
	defer aiUsageMu.Unlock()
	f, err := os.Open(filepath.Join(aiUsageDir(), month+".jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []aiUsageEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e aiUsageEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// aiUsageMonths lists the months that have records, newest first.
func aiUsageMonths() []string {
	entries, _ := os.ReadDir(aiUsageDir())
	var months []string
	for _, e := range entries {
		if m, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && aiUsageMonthR.MatchString(m) {
			months = append(months, m)
		}
	}
	slices.SortFunc(months, func(a, b string) int { return cmp.Compare(b, a) })
	return months
}

// AIUsageRow is one line of a usage table: a purpose, a day, or the total.
type AIUsageRow struct {
	Label         string
	Calls         int
	Input, Output int
	Cost          float64 // USD, from the prices on the page; 0 when none are set
}

// AIUsageReport is the month the AI page shows.
type AIUsageReport struct {
	Month     string
	Total     AIUsageRow
	ByPurpose []AIUsageRow
	ByDay     []AIUsageRow
	ByModel   []AIUsageRow   // which AI the tokens went to — a switch of model changes both count and price
	Recent    []aiUsageEntry // newest first
	Priced    bool
	Unpriced  []string // models with no price yet, whose calls count as $0 until the AI has priced them
	dayModel  map[string]map[string]int
	// Days is every day of the month up to today, quiet ones as zero, oldest
	// first — what the page's charts draw (company-ai-usage-charts.ts).
	Days []AIUsageDay
}

// AIUsageDay is one bar of the daily chart.
type AIUsageDay struct {
	Day    string         `json:"day"` // "10-06"
	Input  int            `json:"in"`
	Output int            `json:"out"`
	Calls  int            `json:"calls"`
	Cost   float64        `json:"cost"`
	Models map[string]int `json:"models"` // tokens per model, for the stacked-by-model view
}

// ChartJSON is the report as the page's charts read it.
func (r AIUsageReport) ChartJSON() string {
	type purpose struct {
		Label  string  `json:"label"`
		Tokens int     `json:"tokens"`
		Calls  int     `json:"calls"`
		Cost   float64 `json:"cost"`
	}
	rows := func(in []AIUsageRow) []purpose {
		out := make([]purpose, 0, len(in))
		for _, p := range in {
			out = append(out, purpose{Label: p.Label, Tokens: p.Input + p.Output, Calls: p.Calls, Cost: p.Cost})
		}
		return out
	}
	b, _ := json.Marshal(map[string]any{"days": r.Days, "purposes": rows(r.ByPurpose), "models": rows(r.ByModel), "priced": r.Priced})
	return string(b)
}

func tokenCost(in, out int, priceIn, priceOut float64) float64 {
	return float64(in)/1e6*priceIn + float64(out)/1e6*priceOut
}

// summarizeAIUsage totals a month's calls, each at its own model's price: what
// was stored when the call was made, else what price prices that model at now.
func summarizeAIUsage(month string, entries []aiUsageEntry, price func(model string) (ModelPrice, bool)) AIUsageReport {
	report := AIUsageReport{Month: month, Total: AIUsageRow{Label: month}}
	unpriced := map[string]bool{}
	costOf := func(e aiUsageEntry) float64 {
		if e.Cost != nil {
			report.Priced = true
			return *e.Cost
		}
		if p, ok := price(e.Model); ok {
			report.Priced = true
			return tokenCost(e.Input, e.Output, p.In, p.Out)
		}
		unpriced[e.Model] = true
		return 0
	}
	add := func(r *AIUsageRow, e aiUsageEntry, cost float64) {
		r.Calls++
		r.Input += e.Input
		r.Output += e.Output
		r.Cost += cost
	}
	byPurpose, byDay, byModel := map[string]*AIUsageRow{}, map[string]*AIUsageRow{}, map[string]*AIUsageRow{}
	report.dayModel = map[string]map[string]int{}
	for _, e := range entries {
		cost := costOf(e)
		add(&report.Total, e, cost)
		if byPurpose[e.Purpose] == nil {
			byPurpose[e.Purpose] = &AIUsageRow{Label: e.Purpose}
		}
		add(byPurpose[e.Purpose], e, cost)
		model := cmp.Or(e.Model, "-")
		if byModel[model] == nil {
			byModel[model] = &AIUsageRow{Label: model}
		}
		add(byModel[model], e, cost)
		day := time.Unix(e.At, 0).Format("2006-01-02")
		if byDay[day] == nil {
			byDay[day] = &AIUsageRow{Label: day}
		}
		add(byDay[day], e, cost)
		if report.dayModel[day] == nil {
			report.dayModel[day] = map[string]int{}
		}
		report.dayModel[day][model] += e.Input + e.Output
	}
	byTokens := func(a, b AIUsageRow) int { return cmp.Compare(b.Input+b.Output, a.Input+a.Output) }
	for _, r := range byPurpose {
		report.ByPurpose = append(report.ByPurpose, *r)
	}
	slices.SortFunc(report.ByPurpose, byTokens)
	for _, r := range byModel {
		report.ByModel = append(report.ByModel, *r)
	}
	slices.SortFunc(report.ByModel, byTokens)
	for m := range unpriced {
		report.Unpriced = append(report.Unpriced, m)
	}
	slices.Sort(report.Unpriced)
	for _, r := range byDay {
		report.ByDay = append(report.ByDay, *r)
	}
	slices.SortFunc(report.ByDay, func(a, b AIUsageRow) int { return cmp.Compare(b.Label, a.Label) })
	report.Days = usageDays(month, byDay, report.dayModel, time.Now())
	recent := slices.Clone(entries)
	slices.Reverse(recent)
	report.Recent = recent[:min(len(recent), 20)]
	return report
}

// Display helpers for the page.

func (r AIUsageRow) InputText() string  { return groupDigits(r.Input) }
func (r AIUsageRow) OutputText() string { return groupDigits(r.Output) }
func (r AIUsageRow) CostText() string   { return fmt.Sprintf("$%.4f", r.Cost) }

func (e aiUsageEntry) When() string       { return time.Unix(e.At, 0).Format("01-02 15:04") }
func (e aiUsageEntry) InputText() string  { return groupDigits(e.Input) }
func (e aiUsageEntry) OutputText() string { return groupDigits(e.Output) }

// groupDigits writes 1234567 as 1,234,567.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + groupDigits(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// usageDays lays the month out day by day, through today for the month in
// progress, so the chart's axis is the calendar rather than the busy days.
func usageDays(month string, byDay map[string]*AIUsageRow, byModel map[string]map[string]int, now time.Time) []AIUsageDay {
	first, err := time.ParseInLocation("2006-01", month, time.Local)
	if err != nil {
		return nil
	}
	last := first.AddDate(0, 1, -1)
	if now.Format("2006-01") == month {
		last = now
	}
	var days []AIUsageDay
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		day := AIUsageDay{Day: d.Format("01-02")}
		if r := byDay[d.Format("2006-01-02")]; r != nil {
			day.Input, day.Output, day.Calls, day.Cost = r.Input, r.Output, r.Calls, r.Cost
			day.Models = byModel[d.Format("2006-01-02")]
		}
		days = append(days, day)
	}
	return days
}
