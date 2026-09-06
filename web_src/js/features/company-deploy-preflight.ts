// "Will this actually deploy?", asked before the request is written rather
// than answered by a failed build after an admin has already approved it.
//
// Driven by a button rather than run on page load: the check resolves the
// dependency tree with pip, which takes seconds, and a page that stalls every
// time it opens is worse than one that answers when asked. It also means
// someone can fix a version, push, and ask again without losing what they
// have typed into the form.

import {POST} from '../modules/fetch.ts';
import {showErrorToast} from '../modules/toast.ts';

type PreflightCheck = {label: string, status: string, detail: string};
type PreflightResult = {
  deployable: boolean,
  summary: string,
  checks: PreflightCheck[],
  needsApproval: string[],
};

const STATUS_CLASS: Record<string, string> = {
  ok: 'company-preflight-ok',
  warn: 'company-preflight-warn',
  error: 'company-preflight-error',
};

const STATUS_LABEL: Record<string, string> = {
  ok: '확인됨',
  warn: '확인 필요',
  error: '고쳐야 함',
};

// Built as DOM rather than as markup: `detail` carries pip's own words and a
// package name the department wrote, and neither is ours to trust into HTML.
function renderResult(panel: HTMLElement, result: PreflightResult): void {
  panel.replaceChildren();

  const summary = document.createElement('div');
  summary.className = `ui message ${result.deployable ? 'positive' : 'negative'}`;
  summary.textContent = result.summary;
  panel.append(summary);

  const list = document.createElement('div');
  list.className = 'company-preflight-list';
  for (const check of result.checks) {
    const row = document.createElement('div');
    row.className = 'company-preflight-row';

    const status = document.createElement('span');
    status.className = STATUS_CLASS[check.status] ?? '';
    status.textContent = STATUS_LABEL[check.status] ?? check.status;

    const label = document.createElement('strong');
    label.textContent = check.label;

    const detail = document.createElement('div');
    detail.className = 'company-preflight-detail';
    // Multi-line: a resolver failure is several lines and reads as one
    // sentence without this.
    detail.textContent = check.detail;

    row.append(status, label, detail);
    list.append(row);
  }
  panel.append(list);
}

export function initCompanyDeployPreflight(): void {
  const button = document.querySelector<HTMLButtonElement>('[data-company-preflight]');
  const panel = document.querySelector<HTMLElement>('[data-company-preflight-result]');
  if (!button || !panel) return;

  const url = button.getAttribute('data-company-preflight') ?? '';
  button.addEventListener('click', async (e) => {
    e.preventDefault();
    button.disabled = true;
    const original = button.textContent;
    button.textContent = '검사하는 중…';
    panel.replaceChildren();
    try {
      const response = await POST(url);
      if (!response.ok) throw new Error(String(response.status));
      renderResult(panel, await response.json());
    } catch {
      // The check is a convenience; failing it must not imply the deploy
      // request itself is unavailable.
      showErrorToast('지금은 검사할 수 없습니다. 배포 요청은 그대로 제출할 수 있습니다.');
    } finally {
      button.disabled = false;
      button.textContent = original;
    }
  });
}
