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
    let reason: string | null | undefined;
    try {
      const resp = await GET(url);
      if (!resp.ok) return;
      ({status, date, reason} = await resp.json() as {status: string | null, date: number | null, reason?: string | null});
    } catch {
      return;
    }
    if (!status) return; // never requested — stays hidden

    const label = el.getAttribute(`data-label-${status}`);
    if (!label) return;
    el.textContent = date ? `${label} - ${formatDatetime(date * 1000)}` : label;
    el.classList.add(status);
    el.classList.remove('tw-hidden');
    // Setting data-tooltip-content here (rather than up front in the
    // template) is enough — modules/tippy.ts watches for it via
    // MutationObserver and wires the tooltip up on its own. Only rejected
    // ever carries a reason (see DeployStatus, company/deploystatus.go);
    // this is the one place on the repo page a requester sees why, without
    // opening /deploy.
    if (reason) el.setAttribute('data-tooltip-content', reason);
  });
}
