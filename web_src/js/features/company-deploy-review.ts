import {registerGlobalInitFunc} from '../modules/observer.ts';
import {fillModelOptions, initChatPanel} from './company-ai-chat.ts';
import {svg} from '../svg.ts';

// Every review the platform posts starts with this (company/deploy.go's
// aiReviewCommentMarker). It is posted by an administrator's account, since
// a comment needs an author — but it was written by a model, and a page
// that shows it under a person's name and avatar says otherwise.
const AI_REVIEW_MARKER = '🤖 AI Review';

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
    if (!body?.textContent?.trimStart().startsWith(AI_REVIEW_MARKER)) continue;
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
