import {registerGlobalInitFunc} from '../modules/observer.ts';
import {GET} from '../modules/fetch.ts';
import {formatDatetime} from '../utils/time.ts';

// Populates the "Deploy: Pending/Approved/Rejected - <date>" badge on the
// repo code page (custom/templates/repo/view_content.tmpl) from
// company/deploystatus.go. Fetched client-side rather than computed in
// the native repo.Home handler, to avoid a core-file touch there — see
// docs/company/repo-ui.md.
export function initCompanyDeployStatus(): void {
  registerGlobalInitFunc('initCompanyDeployStatusBadge', async (el: HTMLElement) => {
    const url = el.getAttribute('data-status-url')!;
    let status: string | null;
    let date: number | null; // unix seconds
    try {
      const resp = await GET(url);
      if (!resp.ok) return;
      ({status, date} = await resp.json() as {status: string | null, date: number | null});
    } catch {
      return;
    }
    if (!status) return; // never requested — stays hidden

    const label = el.getAttribute(`data-label-${status}`);
    if (!label) return;
    el.textContent = date ? `${label} - ${formatDatetime(date * 1000)}` : label;
    el.classList.add(status);
    el.classList.remove('tw-hidden');
  });
}
