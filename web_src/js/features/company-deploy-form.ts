import {registerGlobalInitFunc} from '../modules/observer.ts';

// Powers the "Files included" section on the Deploy Request page
// (custom/templates/company/deploy.tmpl): per-file collapse, a path
// filter, Added/Modified/Removed toggles, and revealing files beyond the
// server's initial render cap (company/deploy.go's deployFilesVisibleLimit).
// Everything here works over what the server already rendered — no extra
// requests — so filtering/searching always sees every file regardless of
// whether "load remaining" has been clicked yet.
export function initCompanyDeployForm(): void {
  registerGlobalInitFunc('initCompanyDeployForm', (el: HTMLElement) => {
    const files = Array.from(el.querySelectorAll<HTMLElement>(':scope > .company-deploy-file'));
    if (!files.length) return;

    const searchInput = el.querySelector<HTMLInputElement>('.company-deploy-filter-input');
    const filterButtons = Array.from(el.querySelectorAll<HTMLButtonElement>('.company-deploy-flt'));
    const collapseAllBtn = el.querySelector<HTMLButtonElement>('.company-deploy-collapse-all');
    const loadMoreBtn = el.querySelector<HTMLButtonElement>('.company-deploy-loadmore-btn');
    const loadMoreContainer = loadMoreBtn?.closest<HTMLElement>('.company-deploy-loadmore');
    const noneEl = el.querySelector<HTMLElement>('.company-deploy-none');

    const collapseAllLabel = el.getAttribute('data-i18n-collapse-all')!;
    const expandAllLabel = el.getAttribute('data-i18n-expand-all')!;
    const noneMatchingSearchTpl = el.getAttribute('data-i18n-none-matching-search')!;
    const noneMatchingFilterText = el.getAttribute('data-i18n-none-matching-filter')!;

    let activeType = 'all';
    let moreRevealed = false;

    for (const file of files) {
      file.querySelector<HTMLElement>('.company-deploy-fhead')!.addEventListener('click', (e: MouseEvent) => {
        if ((e.target as HTMLElement).closest('.company-deploy-ext')) return; // the external-file-view icon shouldn't also toggle the row
        file.classList.toggle('open');
      });
    }

    // Label reflects what the button would do next, not a fixed word — so
    // it stays correct whether files start open or collapsed (currently
    // collapsed; see deploy.tmpl) without this file needing to know which.
    function updateCollapseAllLabel(): void {
      if (!collapseAllBtn) return;
      const anyOpen = files.some((f) => f.classList.contains('open'));
      collapseAllBtn.textContent = anyOpen ? collapseAllLabel : expandAllLabel;
    }

    collapseAllBtn?.addEventListener('click', () => {
      const anyOpen = files.some((f) => f.classList.contains('open'));
      for (const f of files) f.classList.toggle('open', !anyOpen);
      updateCollapseAllLabel();
    });

    loadMoreBtn?.addEventListener('click', () => {
      moreRevealed = true;
      apply();
    });

    function apply(): void {
      const term = (searchInput?.value ?? '').trim().toLowerCase();
      // A search or a non-"all" type filter means the user is looking for
      // something specific — the visible-by-default cap shouldn't hide a
      // match just because "load remaining" was never clicked.
      const filtering = activeType !== 'all' || term !== '';
      let visible = 0;
      for (const file of files) {
        const matchesType = activeType === 'all' || file.getAttribute('data-type') === activeType;
        const matchesSearch = !term || (file.getAttribute('data-path') ?? '').toLowerCase().includes(term);
        const withinInitialLimit = filtering || moreRevealed || file.getAttribute('data-more') !== '1';
        const show = matchesType && matchesSearch && withinInitialLimit;
        file.classList.toggle('tw-hidden', !show);
        if (show) visible++;
      }
      // The button's own "N files" count is the total beyond the cap,
      // computed once server-side (company/deploy.go) — accurate only
      // against the unfiltered "All" view. Filtering/searching already
      // ignores the cap entirely above, so once either is active the
      // button no longer refers to anything still hidden; keeping it
      // visible then just contradicts what the list is already showing.
      loadMoreContainer?.classList.toggle('tw-hidden', filtering || moreRevealed);
      if (noneEl) {
        noneEl.classList.toggle('tw-hidden', visible !== 0);
        noneEl.textContent = term ? noneMatchingSearchTpl.replace('%s', searchInput!.value.trim()) : noneMatchingFilterText;
      }
    }

    searchInput?.addEventListener('input', apply);
    for (const btn of filterButtons) {
      btn.addEventListener('click', () => {
        for (const b of filterButtons) b.classList.remove('on');
        btn.classList.add('on');
        activeType = btn.getAttribute('data-type')!;
        apply();
      });
    }

    // Each collapsed run of unchanged context lines (company/deploy.go's
    // groupDiffSegments) is its own <tbody>: the expand button plus every
    // row it hides, already rendered — clicking it just un-hides its own
    // sibling rows, same "reveal what the server already sent" pattern as
    // "load remaining files" above, no request needed.
    for (const btn of el.querySelectorAll<HTMLButtonElement>('.company-deploy-diff-expand-btn')) {
      btn.addEventListener('click', () => {
        const tbody = btn.closest('tbody')!;
        btn.closest('tr')!.remove();
        for (const row of tbody.querySelectorAll<HTMLElement>('tr.tw-hidden')) row.classList.remove('tw-hidden');
      });
    }

    updateCollapseAllLabel();
    apply();
  });
}

// Submitting a deploy request is slow — the platform resolves the dependency
// tree with pip before it writes anything — and a form that looks inert for
// several seconds gets clicked again.
//
// A second click is not a cosmetic problem: each submission opens a pull
// request and supersedes the previous one, so double-clicking creates a
// request and immediately cancels it, leaving the department waiting on
// something an admin already saw closed.
//
// Deliberately not a fake progress bar. The submission is an ordinary form
// POST, so the page has no way to know which step the server is on, and
// inventing stages would tell someone something the platform does not know.
// An indeterminate indicator with an honest sentence is what there is.
export function initCompanyDeploySubmit(): void {
  const form = document.querySelector<HTMLFormElement>('#deploy-form');
  const button = form?.querySelector<HTMLButtonElement>('#submit-button');
  if (!form || !button) return;

  let submitting = false;
  form.addEventListener('submit', () => {
    // Only fires once the browser's own validation has passed, so an
    // incomplete form never gets stuck showing "submitting".
    if (submitting) return;
    submitting = true;

    // After the event has begun: the form data is already serialized by now,
    // so disabling the button cannot drop a field.
    button.classList.add('is-loading');
    button.disabled = true;

    const note = document.createElement('div');
    note.className = 'text-grey tw-text-13 tw-mt-2';
    note.textContent = '배포할 수 있는 상태인지 확인하고 요청을 만드는 중입니다. 몇 초 걸릴 수 있습니다.';
    button.insertAdjacentElement('afterend', note);
  });
}
