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
import {POST} from '../modules/fetch.ts';
import {toggleElem} from '../utils/dom.ts';
import {htmlEscape} from '../utils/html.ts';
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
  dataMB: number;
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
        {label: text('data'), data: at('dataMB'), borderColor: chartJsColors.text, borderDash: [3, 3], tension: 0.3, pointRadius: 0},
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

// A limit card on the admin app page: the pencil swaps the number after
// the slash for a field on the same line; the tick (or Enter) submits the
// card's own form, the cross (or Escape) puts the number back. The field
// is disabled while closed so a stray Enter elsewhere cannot submit it.
export function initCompanyKpiEdit() {
  for (const form of document.querySelectorAll<HTMLFormElement>('form.company-kpi-edit')) {
    const text = form.querySelector<HTMLElement>('.company-kpi-limit-text');
    const input = form.querySelector<HTMLInputElement>('.company-kpi-limit-input');
    const pencil = form.querySelector<HTMLButtonElement>('.company-kpi-pencil');
    const actions = form.querySelector<HTMLElement>('.company-kpi-limit-actions');
    const unit = form.querySelector<HTMLElement>('.company-kpi-limit-unit'); // the data card shows "128 MiB" but is edited in MB
    if (!text || !input || !pencil || !actions) continue;
    const original = input.value;
    const open = (on: boolean) => {
      text.classList.toggle('tw-hidden', on);
      pencil.classList.toggle('tw-hidden', on);
      input.classList.toggle('tw-hidden', !on);
      actions.classList.toggle('tw-hidden', !on);
      unit?.classList.toggle('tw-hidden', !on);
      input.disabled = !on;
      if (on) {
        input.focus();
        input.select();
      } else {
        input.value = original;
      }
    };
    pencil.addEventListener('click', () => open(true));
    form.querySelector('.company-kpi-cancel')!.addEventListener('click', () => open(false));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') open(false);
    });
  }
}

// The app list: the header checkbox ticks every row, and the bulk buttons
// appear with a count once something is ticked. The owner and sort filters
// are plain links (custom/templates/company/admin_deploys.tmpl) and need
// nothing from here.
export function initCompanyFleet() {
  const bulk = document.querySelector<HTMLFormElement>('#fleet-bulk');
  if (!bulk) return;
  const all = bulk.querySelector<HTMLInputElement>('.company-fleet-check-all');
  const rows = Array.from(bulk.querySelectorAll<HTMLInputElement>('.company-fleet-check-row'));
  const buttons = bulk.querySelector<HTMLElement>('.company-fleet-bulk');
  const foot = bulk.querySelector<HTMLElement>('.company-fleet-foot > span');
  const template = foot?.textContent ?? '';
  const sync = () => {
    const n = rows.filter((r) => r.checked).length;
    buttons?.classList.toggle('tw-hidden', n === 0);
    if (foot) foot.textContent = template.replace(/\d+(?=\D*$)/, String(n));
    if (all) all.indeterminate = n > 0 && n < rows.length;
  };
  all?.addEventListener('change', () => {
    for (const r of rows) r.checked = all.checked;
    sync();
  });
  for (const r of rows) r.addEventListener('change', sync);
}

// Destructive controls (stop, remove, rollback) ask first. Stopping an app
// disconnects whoever is using it right now, which is not obvious from a
// button labelled "중지".
// The mail body, seen as HTML. The server renders it through the same
// substitution the real message uses, and it is shown in a sandboxed frame:
// the administrator's own markup should neither run in this page nor inherit
// its styles — a preview that looks like Gitea is not a preview of a mail.
export function initCompanyMailPreview() {
  for (const field of document.querySelectorAll<HTMLElement>('.company-mail-body')) {
    const textarea = field.querySelector<HTMLTextAreaElement>('textarea')!;
    const frame = field.querySelector<HTMLIFrameElement>('.company-mail-preview')!;
    const tabs = field.querySelectorAll<HTMLButtonElement>('[data-mail-tab]');
    const url = field.getAttribute('data-preview-url')!;

    const show = async (which: string) => {
      for (const tab of tabs) tab.classList.toggle('active', tab.getAttribute('data-mail-tab') === which);
      const preview = which === 'preview';
      toggleElem(textarea, !preview);
      toggleElem(frame, preview);
      if (!preview) return;
      frame.srcdoc = '';
      const body = new FormData();
      body.append('body', textarea.value);
      let html: string;
      try {
        const resp = await POST(url, {data: body});
        if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`);
        ({html} = await resp.json());
      } catch (e) {
        // Said in the frame rather than swallowed: an empty preview looks
        // exactly like a body that renders to nothing, and "it stopped
        // working" is not something anyone can act on.
        frame.srcdoc = `<!DOCTYPE html><meta charset="utf-8"><body style="color-scheme:light;margin:0;padding:12px;font:14px system-ui;color:#b91c1c;background:#fff">${htmlEscape(String(e))}</body>`;
        return;
      }
      // A document of its own, so the mail's own markup decides how it looks.
      //
      // color-scheme and the colours are pinned to light on purpose: an iframe
      // with no scheme of its own takes the embedder's, so on a dark page the
      // browser rendered this text white — on the white background a mail is
      // read against. A mail client is not the admin panel's theme.
      const subject = (field.closest('form')?.querySelector<HTMLInputElement>('[name="subject"]')?.value ?? '').trim();
      const head = subject ? `<div style="border-bottom:1px solid #ddd;padding-bottom:8px;margin-bottom:12px;font-weight:600">${htmlEscape(subject)}</div>` : '';
      frame.srcdoc = `<!DOCTYPE html><meta charset="utf-8">` +
        `<body style="color-scheme:light;margin:0;padding:12px;font:14px/1.5 system-ui;color:#1f2328;background:#fff">${head}${html}</body>`;
    };
    for (const tab of tabs) tab.addEventListener('click', () => show(tab.getAttribute('data-mail-tab')!));
  }
}

// A list of addresses built one at a time: type, press Enter, it becomes an
// item with an × on it. Each item carries a hidden input of the given name,
// so the form posts a list and the server parses nothing.
export function initCompanyTagInput() {
  for (const box of document.querySelectorAll<HTMLElement>('[data-company-tags]')) {
    const name = box.getAttribute('data-company-tags')!;
    const entry = box.querySelector<HTMLInputElement>('.company-tag-entry');
    if (!entry) continue;

    const values = () => Array.from(box.querySelectorAll<HTMLInputElement>(`input[name="${CSS.escape(name)}"]`), (el) => el.value.toLowerCase());
    const add = (raw: string) => {
      // Split here too: people paste a list into the box they were told to
      // type one address into.
      for (const part of raw.split(/[\n,;]+/)) {
        const value = part.trim();
        if (!value || values().includes(value.toLowerCase())) continue;
        const tag = document.createElement('span');
        tag.className = 'company-tag';
        const hidden = document.createElement('input');
        hidden.type = 'hidden';
        hidden.name = name;
        hidden.value = value;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.className = 'company-tag-remove';
        remove.textContent = '×';
        tag.append(hidden, document.createTextNode(value), remove);
        box.insertBefore(tag, entry);
      }
      entry.value = '';
    };

    entry.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ',') {
        e.preventDefault(); // Enter in a text field would submit the form
        add(entry.value);
      } else if (e.key === 'Backspace' && !entry.value) {
        box.querySelector('.company-tag:last-of-type')?.remove();
      }
    });
    // Leaving the field commits what is in it: somebody who types an address
    // and presses Save should not lose it for want of an Enter.
    entry.addEventListener('blur', () => add(entry.value));
    entry.form?.addEventListener('submit', () => add(entry.value));
    box.addEventListener('click', (e) => {
      const target = e.target as HTMLElement;
      if (target.classList.contains('company-tag-remove')) target.closest('.company-tag')?.remove();
      else if (target === box) entry.focus();
    });
  }
}

// A checkbox that switches another one off with it. The dependent control
// decides nothing while its parent is unchecked, and a control that decides
// nothing should not invite a decision — see custom/templates/company/admin_settings.tmpl.
export function initCompanyDependentToggles() {
  for (const parent of document.querySelectorAll<HTMLInputElement>('input[data-company-enables]')) {
    const child = document.querySelector<HTMLInputElement>(parent.getAttribute('data-company-enables')!);
    if (!child) continue;
    const sync = () => {
      child.disabled = !parent.checked;
      child.closest('.field')?.classList.toggle('disabled', child.disabled);
    };
    parent.addEventListener('change', sync);
    sync();
  }
}

export function initCompanyConfirmForms() {
  for (const form of document.querySelectorAll<HTMLFormElement>('form[data-company-confirm]')) {
    form.addEventListener('submit', (e) => {
      if (!window.confirm(form.getAttribute('data-company-confirm')!)) e.preventDefault();
    });
  }
}
