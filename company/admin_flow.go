// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
	"gitea.dev/services/context"
)

// The admin deploys page draws the platform as a tree: Gitea, the app proxy,
// and one box per app holding everything about it. Positions are worked out
// here so the template only places boxes.
const (
	flowNodeW    = 216
	flowNodeH    = 152
	flowAppW     = 456
	flowAppH     = 120
	flowColGap   = 32
	flowRowGap   = 22 // room for the badges hanging under a box
	flowPadding  = 16
	flowErrorPct = 5.0
	flowWarnPct  = 70
	flowHotPct   = 90
)

type flowBadge struct {
	Text string
	Warn bool
}

// flowMetric is one row inside a box: a label, a gauge when Gauge, a value.
type flowMetric struct {
	Label string
	Value string
	Gauge bool
	Pct   int
	Level string // ok, warn, hot: the gauge's colour
}

func textMetric(label, value string, hot bool) flowMetric {
	return flowMetric{Label: label, Value: value, Level: util.Iif(hot, "hot", "")}
}

// gaugeMetric fills pct of the bar; warnAt and hotAt colour it.
func gaugeMetric(label, value string, pct, warnAt, hotAt int) flowMetric {
	pct = max(min(pct, 100), 0)
	level := "ok"
	switch {
	case pct >= hotAt:
		level = "hot"
	case pct >= warnAt:
		level = "warn"
	}
	return flowMetric{Label: label, Value: value, Gauge: true, Pct: pct, Level: level}
}

// usageMetric is used of total; an unknown total is an empty gauge.
func usageMetric(label string, used, total int64) flowMetric {
	if total <= 0 {
		return flowMetric{Label: label, Value: "—", Gauge: true}
	}
	return gaugeMetric(label, compactBytes(used)+" / "+compactBytes(total), int(used*100/total), flowWarnPct, flowHotPct)
}

// compactBytes keeps a gauge's value short enough to sit beside its bar.
func compactBytes(n int64) string {
	switch {
	case n >= 1024*1024*1024:
		return fmt.Sprintf("%.1fG", float64(n)/(1024*1024*1024))
	case n >= 1024*1024:
		return fmt.Sprintf("%dM", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dK", n/1024)
	}
	return fmt.Sprintf("%dB", n)
}

func errorMetric(label string, rate float64) flowMetric {
	// a fifth of requests failing already fills the bar
	return gaugeMetric(label, fmt.Sprintf("%.1f%%", rate), int(rate*5), 5, flowErrorPct*5)
}

type flowNode struct {
	X, Y    int
	W, H    int
	Kind    string // the label under the icon
	Icon    string
	Name    string
	Link    string
	Health  string // ok, bad, busy, idle; empty draws no mark
	Synced  string // ok, busy; empty draws no mark
	Root    bool
	App     bool // an app's box, wider, its figures in two rows
	Badges  []flowBadge
	Metrics []flowMetric
	Spark   string // request sparkline points, see sparklinePoints
}

type flowEdge struct{ Path string }

type platformFlow struct {
	Width, Height int
	Nodes         []*flowNode
	Edges         []flowEdge
	ApprovalOn    bool
	Down          int // running but not answering
}

// FlowState is the health of an app's box.
func (r *adminDeployRow) FlowState() string {
	switch {
	case r.Kind() == "failed", r.State.Actual == AppStateRunning && r.State.Health.State == "down":
		return "bad"
	case r.State.IsBusy():
		return "busy"
	case r.Kind() == "running":
		return "ok"
	}
	return "idle"
}

// buildPlatformFlow runs after summarizeFleet, which fills the rows' figures.
func buildPlatformFlow(ctx *context.Context, rows []*adminDeployRow) *platformFlow {
	tr := ctx.Locale.TrString
	f := &platformFlow{ApprovalOn: currentDeployApprovalPolicy(ctx).WaivedBy == nil}
	dataOn := AppDataEnabled()

	deployed := make([]*adminDeployRow, 0, len(rows))
	var requests, busiest int
	var errors float64
	for _, r := range rows {
		if r.Repo == nil || r.Kind() == "undeployed" {
			continue
		}
		deployed = append(deployed, r)
		requests += r.Requests
		busiest = max(busiest, r.Requests)
		errors += r.ErrorRate * float64(r.Requests) / 100
		if r.State.Actual == AppStateRunning && r.State.Health.State == "down" {
			f.Down++
		}
	}
	// What needs a look first, then the busiest.
	rank := map[string]int{"bad": 0, "busy": 1, "ok": 2, "idle": 3}
	sort.SliceStable(deployed, func(i, j int) bool {
		a, b := rank[deployed[i].FlowState()], rank[deployed[j].FlowState()]
		if a != b {
			return a < b
		}
		return deployed[i].Requests > deployed[j].Requests
	})

	colX := func(col int) int { return flowPadding + col*(flowNodeW+flowColGap) }
	link := func(parent, child *flowNode) {
		x1, y1 := parent.X+parent.W, parent.Y+parent.H/2
		x2, y2 := child.X, child.Y+child.H/2
		mid := (x1 + x2) / 2
		f.Edges = append(f.Edges, flowEdge{Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, mid, y1, mid, y2, x2, y2)})
	}

	apps := make([]*flowNode, 0, len(deployed))
	for i, r := range deployed {
		apps = append(apps, appNode(tr, r, busiest, dataOn, colX(2), flowPadding+i*(flowAppH+flowRowGap)))
	}
	// Gitea and the proxy sit level with the first apps, not the middle of a
	// long list, so they stay in view above the fold.
	topY := flowPadding
	if n := len(apps); n > 0 {
		topY = (apps[0].Y + apps[min(n, 3)-1].Y + flowAppH - flowNodeH) / 2
	}

	errorRate := 0.0
	if requests > 0 {
		errorRate = errors * 100 / float64(requests)
	}
	guardOn := proxyGuardEnabled()
	banned := guardBannedNow(time.Now())
	proxy := &flowNode{
		X: colX(1), Y: topY, W: flowNodeW, H: flowNodeH, Kind: tr("company.admin.flow_kind_proxy"), Icon: "octicon-shield-check",
		Name: "/apps", Health: util.Iif(guardOn, "ok", "idle"), Link: setting.AppSubURL + "/-/admin/company-proxy",
		Metrics: []flowMetric{
			textMetric(tr("company.admin.flow_m_requests"), strconv.Itoa(requests), false),
			errorMetric(tr("company.admin.flow_m_errors"), errorRate),
			textMetric(tr("company.admin.flow_m_banned"), strconv.Itoa(banned), banned > 0),
		},
	}
	if guardOn {
		proxy.Badges = append(proxy.Badges, flowBadge{Text: tr("company.admin.flow_guard_rules",
			companySettingPositiveInt("APP_PROXY_RATE_PER_MIN", guardRatePerMinDefault),
			companySettingPositiveInt("APP_PROXY_BAN_STRIKES", guardBanStrikesDefault),
			companySettingPositiveInt("APP_PROXY_BAN_MINUTES", guardBanMinutesDefault))})
	} else {
		proxy.Badges = append(proxy.Badges, flowBadge{Text: tr("company.admin.flow_guard_off"), Warn: true})
	}

	mode, _ := sandboxMode()
	root := &flowNode{
		X: colX(0), Y: topY, W: flowNodeW, H: flowNodeH, Icon: "octicon-server", Name: "Gitea", Root: true,
		Health: util.Iif(mode == SandboxNone, "bad", "ok"),
		Badges: []flowBadge{{Text: tr("company.admin.flow_sandbox", string(mode)), Warn: mode == SandboxNone}},
	}
	if s, ok := LatestSystemSample(); ok {
		root.Metrics = []flowMetric{
			gaugeMetric("CPU", fmt.Sprintf("%.0f%%", s.CPU), int(s.CPU), flowWarnPct, flowHotPct),
			// a host that cannot read its memory use reports none, not zero
			usageMetric(tr("company.admin.flow_m_memory"), s.MemUsedMB*1024*1024, util.Iif(s.MemUsedMB > 0, s.MemTotalMB, 0)*1024*1024),
			usageMetric(tr("company.admin.flow_m_disk"), s.DiskUsedMB*1024*1024, s.DiskTotalMB*1024*1024),
		}
	}
	link(root, proxy)
	for _, a := range apps {
		link(proxy, a)
	}
	f.Nodes = append([]*flowNode{root, proxy}, apps...)
	f.Width = colX(2) + util.Iif(len(apps) > 0, flowAppW, 0) + flowPadding
	f.Height = max(topY+flowNodeH, flowPadding+len(apps)*(flowAppH+flowRowGap)-flowRowGap) + flowRowGap + flowPadding
	return f
}

// appNode is one app with everything about it: traffic, what it uses, and
// where it may connect.
func appNode(tr func(string, ...any) string, r *adminDeployRow, busiest int, dataOn bool, x, y int) *flowNode {
	settings := SettingsFor(r.State.Owner, r.State.Repo)
	n := &flowNode{
		X: x, Y: y, W: flowAppW, H: flowAppH, App: true,
		Kind: "Python", Icon: "octicon-rocket",
		Name: r.State.Owner + "/" + r.State.Repo, Link: r.flowLink(),
		Health: r.FlowState(), Spark: r.Sparkline,
	}
	switch currentRuntime(r.State.Owner, r.State.Repo) {
	case RuntimeStatic:
		n.Kind, n.Icon = tr("company.admin.flow_kind_static"), "octicon-browser"
	case RuntimeJava:
		n.Kind, n.Icon = "Java", "octicon-flame"
	}
	switch {
	case r.State.IsBusy():
		n.Synced = "busy"
	case r.State.HasRelease:
		n.Synced = "ok"
	}

	if r.State.Actual != AppStateRunning {
		n.Badges = append(n.Badges, flowBadge{Text: tr("company.status." + r.State.Actual), Warn: r.FlowState() == "bad"})
	}
	n.Badges = append(n.Badges,
		flowBadge{Text: tr("company.admin.flow_access_" + r.Access)},
		flowBadge{Text: tr("company.admin.flow_net_" + r.Network), Warn: r.Network == NetworkOpen},
	)
	if settings.Download.Policy == "allow" {
		n.Badges = append(n.Badges, flowBadge{Text: tr("company.admin.flow_download_allow"), Warn: true})
	}
	if !r.State.Sandboxed && r.State.Actual == AppStateRunning {
		n.Badges = append(n.Badges, flowBadge{Text: tr("company.admin.flow_unsandboxed"), Warn: true})
	}

	cpu := flowMetric{Label: "CPU", Value: "—", Gauge: true}
	mem := flowMetric{Label: tr("company.admin.flow_m_memory"), Value: "— / " + compactBytes(int64(r.MemLimitMB)*1024*1024), Gauge: true}
	if r.Measured {
		cpu = gaugeMetric("CPU", fmt.Sprintf("%d%%", r.CPUPct), r.CPUPct, flowWarnPct, flowHotPct)
		mem = usageMetric(mem.Label, int64(r.MemNowMB)*1024*1024, int64(r.MemLimitMB)*1024*1024)
	}
	disk := flowMetric{Label: tr("company.admin.flow_m_disk"), Value: "—", Gauge: true}
	if dataOn && r.HasData {
		disk = usageMetric(disk.Label, r.Data.Bytes, r.Data.QuotaBytes)
		if r.Data.Warning() {
			disk.Level = "hot"
		}
	}
	n.Metrics = []flowMetric{
		// the busiest app fills the bar: it compares apps, it is not a limit
		gaugeMetric(tr("company.admin.flow_m_requests"), strconv.Itoa(r.Requests), r.Requests*100/max(busiest, 1), 101, 101),
		cpu,
		disk,
		errorMetric(tr("company.admin.flow_m_errors"), r.ErrorRate),
		mem,
		hostsMetric(tr, r.Network, settings),
	}
	return n
}

// hostsMetric is where an app may connect out to.
func hostsMetric(tr func(string, ...any) string, mode string, settings AppSettings) flowMetric {
	label := tr("company.admin.flow_m_hosts")
	switch mode {
	case NetworkOpen:
		return textMetric(label, tr("company.admin.flow_hosts_any"), true)
	case NetworkBroker:
		hosts := make([]string, 0, len(settings.Network.Allow))
		for _, rule := range settings.Network.Allow {
			hosts = append(hosts, rule.Host)
		}
		if len(hosts) == 0 {
			return textMetric(label, tr("company.admin.flow_hosts_none"), false)
		}
		return textMetric(label, strings.Join(hosts, ", "), false)
	}
	return textMetric(label, tr("company.admin.flow_hosts_isolated"), false)
}

func (r *adminDeployRow) flowLink() string {
	return setting.AppSubURL + "/-/admin/company-deploys/" + url.PathEscape(r.State.Owner) + "/" + url.PathEscape(r.State.Repo)
}
