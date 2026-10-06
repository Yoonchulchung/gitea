import {registerGlobalInitFunc} from '../modules/observer.ts';
import {fillModelOptions, initChatPanel} from './company-ai-chat.ts';
import {svg} from '../svg.ts';
import {GET} from '../modules/fetch.ts';

// A preview build takes as long as a deploy's. While it runs, the panel
// asks every few seconds and reloads the page when the answer changes
// (company/apppreview.go).
export function initCompanyDeployPreview() {
  registerGlobalInitFunc('initCompanyDeployPreview', (el: HTMLElement) => {
    if (el.getAttribute('data-state') !== 'building') return;
    const url = el.getAttribute('data-status-url')!;
    const poll = async () => {
      try {
        const resp = await GET(url);
        if (!resp.ok) return;
        const {state} = await resp.json() as {state: string};
        if (state !== 'building') {
          window.location.reload();
          return;
        }
      } catch {
        // a missed poll only delays the reload
      }
      setTimeout(poll, 3000);
    };
    setTimeout(poll, 3000);
  });
}

// Every comment the platform AI posts opens with its card title
// (company/ai_review_card.go); older ones with the emoji marker. It is posted
// by an administrator's account, since a comment needs an author — but it was
// written by a model, and a page that shows it under a person's name and
// avatar says otherwise.
const AI_CARD_TITLE = 'AI 검토 · ';
const LEGACY_AI_REVIEW_MARKER = '🤖 AI Review';

function isAIComment(body: HTMLElement): boolean {
  const text = body.textContent?.trimStart() ?? '';
  return text.startsWith(LEGACY_AI_REVIEW_MARKER) || Boolean(body.querySelector(':scope > h4:first-child')?.textContent?.startsWith(AI_CARD_TITLE));
}

// The snapshot commit, the labels and the lock on a Deploy Request are the
// platform's doing, under the central repository owner's account
// (company/deploy.go). Shown as the owner's, they read as decisions a
// person took.
function markPlatformEvents(label: string, ownerLink: string): void {
  for (const event of document.querySelectorAll<HTMLElement>('.issue-content-left .timeline-item.event')) {
    if (!event.querySelector('.badge .octicon-repo-push, .badge .octicon-tag, .badge .octicon-lock')) continue;
    const author = event.querySelector<HTMLAnchorElement>('.comment-text-line a.tw-font-semibold');
    if (!author || author.getAttribute('href') !== ownerLink) continue;
    event.classList.add('company-platform-event');
    const name = document.createElement('span');
    name.className = 'tw-font-semibold';
    name.textContent = label;
    author.replaceWith(name);
    const avatar = event.querySelector<HTMLElement>('.avatar-with-link');
    if (avatar) avatar.replaceWith(Object.assign(document.createElement('span'), {className: 'company-platform-avatar', innerHTML: svg('octicon-rocket', 12)}));
  }
}

function markAIComments(label: string): void {
  for (const comment of document.querySelectorAll<HTMLElement>('.issue-content-left .timeline-item.comment')) {
    const body = comment.querySelector<HTMLElement>('.comment-body .render-content');
    if (!body || !isAIComment(body)) continue;
    comment.classList.add('company-ai-comment');
    for (const avatar of comment.querySelectorAll<HTMLElement>('.timeline-avatar, .inline-timeline-avatar')) {
      avatar.innerHTML = `<span class="company-ai-avatar">${svg('octicon-sparkle-fill', 20)}</span>`;
      avatar.removeAttribute('href');
    }
    const author = comment.querySelector<HTMLElement>('.comment-header-left a.tw-font-semibold');
    if (author) {
      const name = document.createElement('span');
      name.className = 'tw-font-semibold';
      name.textContent = label;
      author.replaceWith(name);
    }
    for (const role of comment.querySelectorAll('.comment-header-right .role-label')) role.remove();
  }
}

// The assistant on a Deploy Request PR's sidebar
// (custom/templates/company/deploy_review_sidebar.tmpl): the same chat
// panel as the workspace editor's, read-only — the server offers no
// write tools, so no edit events arrive. See company/deploy_review.go.
// The countdown before an AI approval merges (company/auto_approve_wait.go).
// Counts locally between polls; when the server's phase changes — the review
// finished, the merge happened, someone stopped it — the page reloads to show it.
export function initCompanyAutoApprove() {
  registerGlobalInitFunc('initCompanyAutoApprove', (el: HTMLElement) => {
    const statusUrl = el.getAttribute('data-status-url')!;
    const phase = el.getAttribute('data-phase')!;
    if (phase === 'stopped') return;
    // Every countdown on the page: the sidebar's, and the waiting code review's in the timeline.
    const seconds = document.querySelectorAll<HTMLElement>('.company-auto-approve-seconds');
    const mergeAt = Number(el.getAttribute('data-merge-at'));
    const tick = () => {
      if (!mergeAt) return;
      for (const s of seconds) s.textContent = String(Math.max(0, Math.round(mergeAt - Date.now() / 1000)));
    };
    const poll = async () => {
      try {
        const resp = await GET(statusUrl);
        if (!resp.ok) return;
        const status = await resp.json() as {phase: string};
        if (status.phase !== phase) window.location.reload();
      } catch {
        // a missed poll is retried by the next one
      }
    };
    tick();
    setInterval(tick, 1000);
    setInterval(poll, 3000);
  });
}

// The code review under way on a Deploy Request (company/ai_review_progress.go):
// seconds counted here, parts and completion from the server.
export function initCompanyAIReviewProgress() {
  registerGlobalInitFunc('initCompanyAIReviewProgress', (el: HTMLElement) => {
    const startedAt = Number(el.getAttribute('data-started-at'));
    const elapsed = el.querySelector<HTMLElement>('.company-aireview-elapsed')!;
    const parts = el.querySelector<HTMLElement>('.company-aireview-parts')!;
    const partText = el.getAttribute('data-i18n-part')!;
    const tick = () => { elapsed.textContent = String(Math.max(0, Math.round(Date.now() / 1000 - startedAt))) };
    tick();
    setInterval(tick, 1000);
    setInterval(async () => {
      try {
        const resp = await GET(el.getAttribute('data-status-url')!);
        if (!resp.ok) return;
        const status = await resp.json() as {phase: string, done: number, total: number};
        if (status.phase !== 'running') {
          window.location.reload(); // the comments are in, or it failed and says why
          return;
        }
        if (status.total > 1) parts.textContent = `· ${partText.replace('%s', () => String(status.done)).replace('%s', () => String(status.total))}`;
      } catch {
        // the next poll tries again
      }
    }, 2000);
  });
}

// The approval queue's countdowns (custom/templates/company/approval_queue.tmpl):
// each ticks on its own, and the list reloads when one runs out or while a
// review is under way, so a row never claims a state it has left.
export function initCompanyQueueCountdown() {
  registerGlobalInitFunc('initCompanyQueueCountdown', (table: HTMLElement) => {
    const countdowns = table.querySelectorAll<HTMLElement>('.company-queue-seconds');
    const reviewing = table.querySelector('.company-queue-ai.reviewing');
    const queued = table.querySelector('.company-queue-ai.queued'); // picked up by the sweeper within a minute
    if (!countdowns.length && !reviewing && !queued) return;
    setInterval(() => {
      for (const el of countdowns) {
        const left = Math.round(Number(el.getAttribute('data-merge-at')) - Date.now() / 1000);
        el.textContent = String(Math.max(0, left));
        if (left <= -2) window.location.reload(); // merged by now: show it
      }
    }, 1000);
    if (reviewing || queued) setTimeout(() => window.location.reload(), reviewing ? 5000 : 20000);
  });
}

export function initCompanyDeployReview() {
  registerGlobalInitFunc('initCompanyDeployReview', (el: HTMLElement) => {
    const modelSelect = el.querySelector<HTMLSelectElement>('.company-ai-chat-model');
    initChatPanel(el, {
      sendUrl: el.getAttribute('data-send-url')!,
      emptyStateText: el.getAttribute('data-ai-empty-state')!,
      getContext: () => ({activePath: null, openFiles: [], model: modelSelect?.value ?? ''}),
    });
    if (modelSelect) fillModelOptions(modelSelect);
  });
}

// The packages ticked in the sidebar go with "Approve deploy". They sit
// outside the merge form (Gitea's own, mounted by Vue in the other
// column), so they are copied in as hidden fields the moment it submits —
// on capture, ahead of the fetch that serialises it. The "allow all" box
// drives the rest and follows them.
export function initCompanyDeployReviewPackages() {
  registerGlobalInitFunc('initCompanyDeployReviewPackages', (el: HTMLElement) => {
    const allBox = el.querySelector<HTMLInputElement>('.company-deploy-review-allow-all-box');
    const boxes = Array.from(el.querySelectorAll<HTMLInputElement>('.company-deploy-review-package-box'));
    markAIComments(el.getAttribute('data-i18n-ai-author')!);
    markPlatformEvents(el.getAttribute('data-i18n-platform-author')!, el.getAttribute('data-central-owner-link')!);
    if (!boxes.length) return;
    const syncAll = () => {
      if (!allBox) return;
      const ticked = boxes.filter((b) => b.checked).length;
      allBox.checked = ticked === boxes.length;
      allBox.indeterminate = ticked > 0 && ticked < boxes.length;
    };
    allBox?.addEventListener('change', () => {
      for (const b of boxes) b.checked = allBox.checked;
    });
    for (const b of boxes) b.addEventListener('change', syncAll);

    document.addEventListener('submit', (e) => {
      const form = e.target as HTMLFormElement;
      if (!form.matches('form[action$="/merge"]')) return;
      for (const old of form.querySelectorAll('input[name="company_package"]')) old.remove();
      for (const b of boxes) {
        if (!b.checked) continue;
        const input = document.createElement('input');
        input.type = 'hidden';
        input.name = 'company_package';
        input.value = b.value;
        form.append(input);
      }
    }, {capture: true});
  });
}
