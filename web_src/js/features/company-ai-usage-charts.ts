import {
  BarController,
  BarElement,
  CategoryScale,
  Chart,
  Filler,
  Legend,
  LinearScale,
  LineController,
  LineElement,
  PointElement,
  Tooltip,
  type TooltipItem,
} from 'chart.js';
import {chartJsColors} from '../utils/color.ts';
import {registerGlobalInitFunc} from '../modules/observer.ts';

// The platform AI's token usage on /-/admin/company-ai (company/ai_usage.go's
// ChartJSON). Colors are the validated categorical slots 1-3 of the data-viz
// reference palette, stepped separately for Gitea's light and dark themes.

type Day = {day: string, in: number, out: number, calls: number, cost: number, models: Record<string, number> | null};
type Purpose = {label: string, tokens: number, calls: number, cost: number};
type Usage = {days: Day[], purposes: Purpose[], models: Purpose[], priced: boolean};

const palette = {
  light: {input: '#2a78d6', output: '#eb6834', cost: '#4a3aa7'},
  dark: {input: '#3987e5', output: '#d95926', cost: '#9085e9'},
};

// One color per model, in the reference palette's fixed categorical order: a
// model keeps its color in every chart on the page.
const categorical = {
  light: ['#2a78d6', '#eb6834', '#1baf7a', '#eda100', '#e87ba4', '#008300', '#4a3aa7', '#e34948'],
  dark: ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9', '#e66767'],
};

const compact = new Intl.NumberFormat(undefined, {notation: 'compact', maximumFractionDigits: 1});
const whole = new Intl.NumberFormat();
const usd = (v: number) => `$${v < 1 ? v.toFixed(4) : v.toFixed(2)}`;
// Axis ticks need fewer digits than a tooltip.
const usdTick = (v: number) => `$${v === 0 ? '0' : v < 0.01 ? v.toFixed(4) : v < 1 ? v.toFixed(2) : v.toFixed(0)}`;

function shorten(label: string, max = 18): string {
  return label.length > max ? `${label.slice(0, max - 1)}…` : label;
}

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function initCompanyAIUsageCharts() {
  registerGlobalInitFunc('initCompanyAIUsageCharts', (root: HTMLElement) => {
    const usage = JSON.parse(root.getAttribute('data-usage')!) as Usage;
    const text = (name: string) => root.getAttribute(`data-i18n-${name}`)!;
    Chart.register(BarController, BarElement, CategoryScale, Filler, Legend, LinearScale, LineController, LineElement, PointElement, Tooltip);

    const dark = cssVar('--is-dark-theme') === 'true';
    const colors = dark ? palette.dark : palette.light;
    // Biggest model first; past eight, the rest share the last slot rather than a made-up hue.
    const modelNames = usage.models.map((m) => m.label);
    const modelColor = (name: string) => {
      const slots = dark ? categorical.dark : categorical.light;
      return slots[Math.min(modelNames.indexOf(name), slots.length - 1)];
    };
    const byModel = modelNames.length > 1;
    for (const swatch of root.querySelectorAll<HTMLElement>('[data-swatch]')) {
      swatch.style.backgroundColor = colors[swatch.getAttribute('data-swatch') as 'input' | 'output'];
    }
    const surface = cssVar('--color-box-body') || cssVar('--color-body');
    const grid = {color: chartJsColors.border, drawTicks: false};
    const ticks = {color: chartJsColors.text, font: {size: 11}, padding: 6};
    const legend = {
      position: 'top' as const, align: 'start' as const,
      labels: {color: chartJsColors.text, usePointStyle: true, pointStyle: 'circle', boxWidth: 8, boxHeight: 8, padding: 14, font: {size: 12}},
    };
    const tooltip = {padding: 10, boxPadding: 4, usePointStyle: true, titleFont: {size: 12}, bodyFont: {size: 12}};

    const daily = root.querySelector<HTMLCanvasElement>('canvas[data-chart="daily"]');
    if (daily) {
      new Chart(daily, {
        type: 'bar',
        data: {
          labels: usage.days.map((d) => d.day),
          // Two models in a month means the question is which AI spent it: stacked by model then, by input and output otherwise.
          // Segments are parted by a 2px surface line, and the top one carries the rounded end.
          datasets: byModel ?
            modelNames.map((name, i) => ({
              label: name, data: usage.days.map((d) => d.models?.[name] ?? 0), backgroundColor: modelColor(name),
              borderColor: surface, borderWidth: i === 0 ? 0 : {bottom: 2}, borderSkipped: false,
              borderRadius: i === modelNames.length - 1 ? {topLeft: 4, topRight: 4} : 0, stack: 'tokens', maxBarThickness: 28,
            })) :
            [
              {label: text('input'), data: usage.days.map((d) => d.in), backgroundColor: colors.input, borderRadius: 0, stack: 'tokens', maxBarThickness: 28},
              {label: text('output'), data: usage.days.map((d) => d.out), backgroundColor: colors.output, borderColor: surface, borderWidth: {bottom: 2},
                borderSkipped: false, borderRadius: {topLeft: 4, topRight: 4}, stack: 'tokens', maxBarThickness: 28},
            ],
        },
        options: {
          responsive: true, maintainAspectRatio: false,
          interaction: {mode: 'index', intersect: false},
          plugins: {
            legend,
            tooltip: {
              ...tooltip,
              callbacks: {
                label: (item: TooltipItem<'bar'>) => `${item.dataset.label}: ${whole.format(item.parsed.y ?? 0)}`,
                footer: (items: TooltipItem<'bar'>[]) => {
                  const day = usage.days[items[0].dataIndex];
                  const lines = [`${text('total')}: ${whole.format(day.in + day.out)}`, `${text('calls')}: ${day.calls}`];
                  if (usage.priced) lines.push(`${text('cost')}: ${usd(day.cost)}`);
                  return lines;
                },
              },
            },
          },
          scales: {
            x: {stacked: true, grid: {display: false}, ticks: {...ticks, maxRotation: 0, autoSkipPadding: 12}, border: {display: false}},
            y: {stacked: true, beginAtZero: true, grid, border: {display: false}, ticks: {...ticks, maxTicksLimit: 6, callback: (v) => compact.format(Number(v))}},
          },
        },
      });
    }

    const cost = root.querySelector<HTMLCanvasElement>('canvas[data-chart="cost"]');
    if (cost) {
      new Chart(cost, {
        type: 'line',
        data: {
          labels: usage.days.map((d) => d.day),
          datasets: [{
            label: text('cost'), data: usage.days.map((d) => d.cost),
            // Straight segments: a day's cost is one value, not a curve through the night before.
            borderColor: colors.cost, borderWidth: 2, tension: 0, pointHoverRadius: 6,
            pointRadius: usage.days.map((d) => (d.cost > 0 ? 4 : 0)), pointBackgroundColor: colors.cost, pointBorderColor: surface, pointBorderWidth: 2,
            pointHoverBackgroundColor: colors.cost, pointHoverBorderColor: surface, pointHoverBorderWidth: 2,
            fill: true,
            backgroundColor: (c) => {
              const area = c.chart.chartArea;
              if (!area) return 'transparent';
              const g = c.chart.ctx.createLinearGradient(0, area.top, 0, area.bottom);
              g.addColorStop(0, `${colors.cost}55`);
              g.addColorStop(1, `${colors.cost}05`);
              return g;
            },
          }],
        },
        options: {
          responsive: true, maintainAspectRatio: false,
          interaction: {mode: 'index', intersect: false},
          plugins: {legend: {display: false}, tooltip: {...tooltip, callbacks: {label: (item: TooltipItem<'line'>) => `${text('cost')}: ${usd(item.parsed.y ?? 0)}`}}},
          scales: {
            x: {grid: {display: false}, ticks: {...ticks, maxRotation: 0, autoSkipPadding: 16}, border: {display: false}},
            y: {beginAtZero: true, grid, border: {display: false}, ticks: {...ticks, maxTicksLimit: 5, callback: (v) => usdTick(Number(v))}},
          },
        },
      });
    }

    const models = root.querySelector<HTMLCanvasElement>('canvas[data-chart="models"]');
    if (models) {
      new Chart(models, {
        type: 'bar',
        data: {
          labels: modelNames,
          datasets: [{label: text('tokens'), data: usage.models.map((m) => m.tokens), backgroundColor: modelNames.map(modelColor), borderRadius: 4, borderSkipped: 'start', maxBarThickness: 22}],
        },
        options: {
          indexAxis: 'y', responsive: true, maintainAspectRatio: false,
          plugins: {
            legend: {display: false},
            tooltip: {
              ...tooltip,
              callbacks: {
                title: (items: TooltipItem<'bar'>[]) => modelNames[items[0].dataIndex],
                label: (item: TooltipItem<'bar'>) => {
                  const m = usage.models[item.dataIndex];
                  const parts = [`${text('tokens')}: ${whole.format(m.tokens)}`, `${text('calls')}: ${m.calls}`];
                  if (usage.priced) parts.push(`${text('cost')}: ${usd(m.cost)}`);
                  return parts;
                },
              },
            },
          },
          scales: {
            x: {beginAtZero: true, grid, border: {display: false}, ticks: {...ticks, maxTicksLimit: 5, callback: (v) => compact.format(Number(v))}},
            // A dated model ID is long; the axis shows its start, the tooltip the whole of it.
            y: {grid: {display: false}, border: {display: false}, ticks: {...ticks, font: {size: 12}, callback: (_, i) => shorten(modelNames[i])}},
          },
        },
      });
    }

    const purposes = root.querySelector<HTMLCanvasElement>('canvas[data-chart="purposes"]');
    if (purposes) {
      new Chart(purposes, {
        type: 'bar',
        data: {
          labels: usage.purposes.map((p) => p.label),
          datasets: [{label: text('tokens'), data: usage.purposes.map((p) => p.tokens), backgroundColor: colors.input, borderRadius: 4, borderSkipped: 'start', maxBarThickness: 22}],
        },
        options: {
          indexAxis: 'y', responsive: true, maintainAspectRatio: false,
          plugins: {
            legend: {display: false},
            tooltip: {
              ...tooltip,
              callbacks: {
                label: (item: TooltipItem<'bar'>) => {
                  const p = usage.purposes[item.dataIndex];
                  const parts = [`${text('tokens')}: ${whole.format(p.tokens)}`, `${text('calls')}: ${p.calls}`];
                  if (usage.priced) parts.push(`${text('cost')}: ${usd(p.cost)}`);
                  return parts;
                },
              },
            },
          },
          scales: {
            x: {beginAtZero: true, grid, border: {display: false}, ticks: {...ticks, maxTicksLimit: 5, callback: (v) => compact.format(Number(v))}},
            y: {grid: {display: false}, border: {display: false}, ticks: {...ticks, font: {size: 12}}},
          },
        },
      });
    }
  });
}
