import {
  _adapters,
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
  TimeScale,
  Tooltip,
  type Chart as ChartType,
  type Plugin,
} from 'chart.js';
import {chartJsColors} from '../utils/color.ts';
import dayjs from 'dayjs';

// The admin app dashboard's charts. chart.js is already bundled (it drives
// the contributor graphs), so this adds no dependency — see
// docs/company/app-platform.md on the dashboard design.
//
// Not ChartCanvas.vue: that is a Vue component, and these three charts are
// static server-rendered data with no reactivity to justify mounting Vue for.

type Point = {
  t: number;
  requests: number;
  errors: number;
  p95: number;
  memMB: number;
  cpu: number;
  users: number;
};

type Annotation = {t: number; label: string; bad: boolean};

let registered = false;

function registerOnce() {
  if (registered) return;
  registered = true;
  Chart.register(
    BarController, BarElement, CategoryScale, Filler, Legend, LinearScale,
    LineController, LineElement, PointElement, TimeScale, Tooltip,
  );
  Chart.defaults.color = chartJsColors.text;
  Chart.defaults.borderColor = chartJsColors.border;
  // A minimal time adapter, matching ChartCanvas.vue's. Only the units these
  // charts actually ask for are implemented.
  //
  // From the named export, not off Chart: `Chart._adapters` does not exist in
  // chart.js v4, so the cast that made this compile turned a missing property
  // into "Cannot read properties of undefined (reading '_date')" at runtime,
  // on the one page that draws these charts.
  _adapters._date.override({
    // Every unit, not only the ones these charts ask for: chart.js picks the
    // unit from the visible range, so a missing one formats as undefined on
    // whatever zoom level happens to select it. 24-hour and Y-M-D throughout,
    // which is how the rest of these screens write a time.
    formats: () => ({
      datetime: 'YYYY-MM-DD HH:mm:ss',
      millisecond: 'HH:mm:ss.SSS',
      second: 'HH:mm:ss',
      minute: 'HH:mm',
      hour: 'HH:mm',
      day: 'M/D',
      week: 'M/D',
      month: 'YYYY-MM',
      quarter: 'YYYY [Q]Q',
      year: 'YYYY',
    }),
    parse: (v: any) => (dayjs(v).isValid() ? dayjs(v).valueOf() : null),
    format: (t: number, f: string) => dayjs(t).format(f),
    add: (t: number, n: number, u: any) => dayjs(t).add(n, u).valueOf(),
    diff: (a: number, b: number, u: any) => dayjs(a).diff(b, u),
    startOf: (t: number, u: any) => dayjs(t).startOf(u).valueOf(),
    endOf: (t: number, u: any) => dayjs(t).endOf(u).valueOf(),
  });
}

// deployMarkers draws a vertical line at each deploy, rollback, or forced
// stop. This is the single most useful thing on the page: without it an
// admin has to hold the deploy history in their head while reading the
// graph to answer "did this get worse after the last deploy?".
function deployMarkers(annotations: Annotation[]): Plugin {
  return {
    id: 'companyDeployMarkers',
    afterDatasetsDraw(chart: ChartType) {
      const {ctx, chartArea, scales} = chart;
      const x = scales.x;
      if (!x) return;
      // One Path2D per colour, so the whole set of markers is two stroke
      // calls rather than one per annotation.
      const normal = new Path2D();
      const bad = new Path2D();
      for (const a of annotations) {
        const px = x.getPixelForValue(a.t);
        if (px < chartArea.left || px > chartArea.right) continue;
        const path = a.bad ? bad : normal;
        path.moveTo(px, chartArea.top);
        path.lineTo(px, chartArea.bottom);
      }
      ctx.save();
      ctx.setLineDash([4, 3]);
      ctx.lineWidth = 1;
      ctx.strokeStyle = chartJsColors.text;
      ctx.stroke(normal);
      ctx.strokeStyle = chartJsColors.deletions;
      ctx.stroke(bad);
      ctx.restore();
    },
  };
}

const baseOptions = (annotations: Annotation[]) => ({
  responsive: true,
  maintainAspectRatio: false,
  interaction: {mode: 'index' as const, intersect: false},
  plugins: {
    legend: {display: true, labels: {boxWidth: 10, font: {size: 10}}},
    tooltip: {
      callbacks: {
        // Surface the event that happened in this window, so hovering a
        // spike says what caused it rather than only how big it was.
        afterBody: (items: any[]) => {
          const t = items[0]?.parsed?.x;
          if (!t) return '';
          const near = annotations.filter((a) => Math.abs(a.t - t) < 5 * 60 * 1000);
          return near.map((a) => `• ${a.label}`);
        },
      },
    },
  },
  scales: {
    x: {type: 'time' as const, ticks: {maxRotation: 0, font: {size: 10}}, grid: {display: false}},
    y: {beginAtZero: true, ticks: {font: {size: 10}}},
  },
});

function parseJSON<T>(value: string | null, fallback: T): T {
  if (!value) return fallback;
  try {
    return JSON.parse(value) as T;
  } catch {
    // A malformed payload must not take the whole admin page down with it —
    // the numbers above the charts are still readable without them.
    return fallback;
  }
}

export function initCompanyAppCharts() {
  const root = document.querySelector<HTMLElement>('.company-charts');
  if (!root) return;

  const points = parseJSON<Point[]>(root.getAttribute('data-points'), []);
  const annotations = parseJSON<Annotation[]>(root.getAttribute('data-annotations'), []);
  // Series names and the empty state come from the template: this script has
  // no locale of its own, and the same data-* route already carries the
  // points and annotations.
  const text = (name: string) => root.getAttribute(`data-i18n-${name}`) ?? name;
  if (!points.length) {
    root.textContent = text('empty');
    return;
  }

  registerOnce();
  const options = baseOptions(annotations);
  const markers = [deployMarkers(annotations)];
  const at = (key: keyof Point) => points.map((p) => ({x: p.t, y: p[key]}));

  const charts: Record<string, {datasets: any[]}> = {
    requests: {
      datasets: [
        {label: text('requests'), data: at('requests'), borderColor: chartJsColors.commits, tension: 0.3, pointRadius: 0},
        {label: text('errors'), data: at('errors'), borderColor: chartJsColors.deletions, tension: 0.3, pointRadius: 0},
      ],
    },
    resources: {
      datasets: [
        {label: text('memory'), data: at('memMB'), borderColor: chartJsColors.deletions, tension: 0.3, pointRadius: 0},
        {label: text('cpu'), data: at('cpu'), borderColor: chartJsColors.commits, tension: 0.3, pointRadius: 0},
      ],
    },
    latency: {
      datasets: [
        {label: text('p95'), data: at('p95'), borderColor: chartJsColors.commits, tension: 0.3, pointRadius: 0},
      ],
    },
  };

  for (const canvas of root.querySelectorAll<HTMLCanvasElement>('canvas[data-chart]')) {
    const spec = charts[canvas.getAttribute('data-chart') ?? ''];
    if (!spec) continue;
    new Chart(canvas, {type: 'line', data: spec, options, plugins: markers});
  }
}

// Destructive controls (stop, remove, rollback) ask first. Stopping an app
// disconnects whoever is using it right now, which is not obvious from a
// button labelled "중지".
export function initCompanyConfirmForms() {
  for (const form of document.querySelectorAll<HTMLFormElement>('form[data-company-confirm]')) {
    form.addEventListener('submit', (e) => {
      if (!window.confirm(form.getAttribute('data-company-confirm')!)) e.preventDefault();
    });
  }
}

// Log viewers open at the bottom.
//
// The newest lines are the ones anyone came for — a crash loop writes the
// same traceback dozens of times, and landing at the top means scrolling
// past all of it to reach what just happened.
export function initCompanyLogScroll() {
  for (const el of document.querySelectorAll<HTMLElement>('.company-log')) {
    el.scrollTop = el.scrollHeight;
  }
}
