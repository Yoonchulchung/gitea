import {registerGlobalInitFunc} from '../modules/observer.ts';
import {GET} from '../modules/fetch.ts';
import {formatDatetime} from '../utils/time.ts';

// While a deploy is in flight the badge would otherwise sit on "배포 중"
// until the person reloads — and the deploy typically finishes in under a
// minute, so the stale word is on screen for most of the time they're
// looking at it. Re-poll only in that state, and only for a bounded while:
// a deploy that hasn't settled in ~5 minutes is stuck, and quietly polling
// forever would just hide that from everyone.
const DEPLOYING_POLL_MS = 10_000;
const DEPLOYING_POLL_MAX = 30;

type DeployStatusResponse = {
  status: string | null;
  date: number | null;
  reason?: string | null;
};

// Populates the "Deploy: …" badge on the repo code page
// (custom/templates/repo/view_content.tmpl) from company/deploystatus.go.
// Fetched client-side rather than computed in the native repo.Home handler,
// to avoid a core-file touch there — see docs/company/repo-ui.md.
export function initCompanyDeployStatus(): void {
  registerGlobalInitFunc('initCompanyDeployStatusBadge', async (el: HTMLElement) => {
    const url = el.getAttribute('data-status-url')!;

    // Every status this badge has ever shown, so a re-poll can clear the
    // previous one instead of stacking classes (deploying → deployed would
    // otherwise leave the app looking both in-progress and finished).
    const known = [...el.attributes]
      .map((a) => a.name.startsWith('data-label-') ? a.name.slice('data-label-'.length) : '')
      .filter(Boolean);

    const render = (body: DeployStatusResponse): string | null => {
      const {status, date, reason} = body;
      if (!status) return null; // never requested — stays hidden
      const label = el.getAttribute(`data-label-${status}`);
      if (!label) return null;

      el.textContent = date ? `${label} - ${formatDatetime(date * 1000)}` : label;
      el.classList.remove(...known);
      el.classList.add(status);
      el.classList.remove('tw-hidden');

      // Setting data-tooltip-content here (rather than up front in the
      // template) is enough — modules/tippy.ts watches for it via
      // MutationObserver and wires the tooltip up on its own. Only a
      // rejection carries a reason (see DeployStatus, company/deploystatus.go);
      // this is the one place on the repo page a requester sees why,
      // without opening /deploy.
      if (reason) {
        el.setAttribute('data-tooltip-content', reason);
      } else {
        el.removeAttribute('data-tooltip-content'); // a re-poll must not keep a stale reason
      }
      return status;
    };

    const fetchStatus = async (): Promise<DeployStatusResponse | null> => {
      try {
        const resp = await GET(url);
        if (!resp.ok) return null;
        return await resp.json() as DeployStatusResponse;
      } catch {
        return null; // a failed poll is not worth surfacing — the badge just keeps its last value
      }
    };

    const first = await fetchStatus();
    if (!first) return;
    let status = render(first);

    for (let i = 0; status === 'deploying' && i < DEPLOYING_POLL_MAX; i++) {
      await new Promise((resolve) => setTimeout(resolve, DEPLOYING_POLL_MS));
      const next = await fetchStatus();
      if (!next) continue; // transient failure — keep waiting rather than giving up
      status = render(next);
    }
  });
}
